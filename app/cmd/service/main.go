package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/geo-distributed-gateway/sdk/config"
	"github.com/geo-distributed-gateway/sdk/telemetry"
)

func main() {
	serviceName := os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "service-a"
	}
	instance := os.Getenv("INSTANCE_NAME")
	if instance == "" {
		instance = "unknown"
	}
	zone := os.Getenv("ZONE")
	if zone == "" {
		zone = "unknown"
	}

	// Telemetry events  → stdout (JSON Lines, consumed by Events Collector).
	// Operational logs  → stderr (JSON, for log-aggregator / human consumption).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	// OpenTelemetry tracer: exports spans to Jaeger via OTLP HTTP.
	// Empty OTLP_ENDPOINT → no-op shutdown, service still runs (graceful skip).
	shutdownTracer, err := telemetry.InitTracer(context.Background(), serviceName, zone, os.Getenv("OTLP_ENDPOINT"))
	if err != nil {
		slog.Warn("tracer init failed", slog.Any("err", err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracer(ctx); err != nil {
			slog.Warn("tracer shutdown failed", slog.Any("err", err))
		}
	}()

	// currentCfg holds the live ServiceConfig, updated by the watcher goroutine.
	// Zone from env is the baseline; config file can override it and other fields.
	var currentCfg atomic.Value
	currentCfg.Store(config.ServiceConfig{
		Zone:           zone,
		RequestTimeout: 5 * time.Second,
		MaxRetries:     3,
		RetryBackoff:   100 * time.Millisecond,
	})

	// Start config watcher if CONFIG_FILE is provided.
	if cfgFile := os.Getenv("CONFIG_FILE"); cfgFile != "" {
		w, err := config.NewWatcher(cfgFile, 5*time.Second)
		if err != nil {
			slog.Warn("config watcher not started", slog.String("file", cfgFile), slog.Any("err", err))
		} else {
			defer w.Close()

			// Apply initial config: env zone wins if config has no zone override.
			initial := w.Get()
			if initial.Zone == "" {
				initial.Zone = zone
			}
			currentCfg.Store(initial)

			go func() {
				for cfg := range w.Subscribe() {
					prev := currentCfg.Load().(config.ServiceConfig)
					if cfg.Zone == "" {
						cfg.Zone = zone // keep env baseline if file clears the field
					}
					currentCfg.Store(cfg)
					slog.Info("config reloaded",
						slog.String("zone", cfg.Zone),
						slog.Duration("request_timeout", cfg.RequestTimeout),
						slog.Int("max_retries", cfg.MaxRetries),
						slog.Duration("retry_backoff", cfg.RetryBackoff),
						slog.String("prev_zone", prev.Zone),
					)
				}
			}()
		}
	}

	col := telemetry.New(serviceName, instance, zone, nil)
	col.Start()
	defer col.Stop()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		if err := r.Context().Err(); err != nil {
			return // client disconnected or Envoy timed out before we started
		}
		start := time.Now()
		cfg := currentCfg.Load().(config.ServiceConfig)
		userID := r.Header.Get("X-User-ID")
		cabinetID := r.Header.Get("X-Cabinet-ID")
		correlationID := r.Header.Get("X-Correlation-ID")

		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(
			attribute.String("user_id", userID),
			attribute.String("cabinet_id", cabinetID),
			attribute.String("zone", cfg.Zone),
		)

		slog.Info("GET /ping",
			slog.String("instance", instance),
			slog.String("zone", cfg.Zone),
			slog.String("user_id", userID),
			slog.String("cabinet_id", cabinetID),
			slog.String("correlation_id", correlationID),
		)

		w.Header().Set("X-Served-By", instance)
		w.Header().Set("X-Zone", cfg.Zone)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "pong: %s (zone=%s)", instance, cfg.Zone)

		col.Request(userID, cabinetID, correlationID, http.StatusOK, time.Since(start))
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		cfg := currentCfg.Load().(config.ServiceConfig)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"service":  serviceName,
			"instance": instance,
			"zone":     cfg.Zone,
			"config": map[string]any{
				"request_timeout": cfg.RequestTimeout.String(),
				"max_retries":     cfg.MaxRetries,
				"retry_backoff":   cfg.RetryBackoff.String(),
			},
		})
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: otelhttp.NewHandler(mux, serviceName),
	}

	go func() {
		slog.Info("Starting stub service",
			slog.String("addr", srv.Addr),
			slog.String("service", serviceName),
			slog.String("instance", instance),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	// Consul self-registration. Skipped (with a warning) if CONSUL_ADDR is empty
	// or the agent is unreachable — the HTTP server stays up so unit-style /ping
	// probes still work without a discovery backend.
	var (
		consulClient *consulapi.Client
		registered   bool
	)
	if consulAddr := os.Getenv("CONSUL_ADDR"); consulAddr != "" {
		cfg := consulapi.DefaultConfig()
		cfg.Address = consulAddr
		client, err := consulapi.NewClient(cfg)
		if err != nil {
			slog.Warn("consul: client init failed; running without registration",
				slog.String("addr", consulAddr), slog.Any("err", err))
		} else {
			addr := firstNonLoopbackIPv4()
			if addr == "" {
				addr = instance // fallback, but Consul DNS will return CNAME
			}
			reg := &consulapi.AgentServiceRegistration{
				ID:      instance,             // e.g. "service-a-zone1-1"
				Name:    serviceName,          // e.g. "service-a"
				Tags:    []string{zone},       // exactly one of "zone1" / "zone2"
				Address: addr,                 // IP so Consul DNS returns A-record (not CNAME)
				Port:    8080,
				Check: &consulapi.AgentServiceCheck{
					HTTP:                           "http://" + instance + ":8080/health",
					Interval:                       "5s",
					Timeout:                        "2s",
					DeregisterCriticalServiceAfter: "30s",
				},
			}
			if err := client.Agent().ServiceRegister(reg); err != nil {
				slog.Warn("consul: registration failed; running without it",
					slog.String("addr", consulAddr), slog.Any("err", err))
			} else {
				registered = true
				consulClient = client
				slog.Info("consul: registered",
					slog.String("service", serviceName),
					slog.String("instance", instance),
					slog.String("zone", zone),
				)
			}
		}
	}

	// Routing-hints subscriber: listens on Redis Pub/Sub channel
	// "routing:<service>" for ML-derived weights/rate_limits. In v1 we just
	// log received hints — actual application to routing lives in Envoy
	// (Lua filter). The subscriber is independent of the file-watcher:
	// different contracts, different channels, no merging.
	var hintsSub *config.RoutingHintsSubscriber
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		s, err := config.NewRoutingHintsSubscriber(redisURL, serviceName)
		if err != nil {
			slog.Warn("routing hints subscriber failed to init; continuing without it",
				slog.String("redis_url", redisURL), slog.Any("err", err))
		} else {
			hintsSub = s
			go func() {
				for hint := range hintsSub.Updates() {
					slog.Info("routing hints received",
						slog.String("service", hint.Service),
						slog.Int("version", hint.Version),
						slog.Any("weights", hint.Weights),
						slog.Any("rate_limits", hint.RateLimits),
					)
					// v1: log only. Future self-throttling consumers can read
					// from atomic.Value once we settle on a contract.
				}
			}()
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	slog.Info("Shutting down", slog.String("instance", instance))

	if hintsSub != nil {
		if err := hintsSub.Close(); err != nil {
			slog.Warn("routing hints subscriber close", slog.Any("err", err))
		}
	}

	if registered && consulClient != nil {
		if err := consulClient.Agent().ServiceDeregister(instance); err != nil {
			slog.Warn("consul: deregister failed", slog.String("instance", instance), slog.Any("err", err))
		} else {
			slog.Info("consul: deregistered", slog.String("instance", instance))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", slog.Any("err", err))
	}
}

func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}
