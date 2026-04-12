package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/geo-distributed-gateway/sdk/config"
	"github.com/geo-distributed-gateway/sdk/telemetry"
)

func main() {
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

	col := telemetry.New("stub-service", instance, zone, nil)
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
		Handler: mux,
	}

	go func() {
		slog.Info("Starting stub service", slog.String("addr", srv.Addr), slog.String("instance", instance))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	slog.Info("Shutting down", slog.String("instance", instance))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", slog.Any("err", err))
	}
}
