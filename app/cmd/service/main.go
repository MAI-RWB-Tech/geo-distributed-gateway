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
	"syscall"
	"time"

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

	col := telemetry.New("stub-service", instance, zone, nil)
	col.Start()
	defer col.Stop()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		if err := r.Context().Err(); err != nil {
			return // client disconnected or Envoy timed out before we started
		}
		start := time.Now()
		userID := r.Header.Get("X-User-ID")
		cabinetID := r.Header.Get("X-Cabinet-ID")
		correlationID := r.Header.Get("X-Correlation-ID")

		slog.Info("GET /ping",
			slog.String("instance", instance),
			slog.String("user_id", userID),
			slog.String("cabinet_id", cabinetID),
			slog.String("correlation_id", correlationID),
		)

		w.Header().Set("X-Served-By", instance)
		w.Header().Set("X-Zone", zone)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "pong: %s (zone=%s)", instance, zone)

		col.Request(userID, cabinetID, correlationID, http.StatusOK, time.Since(start))
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":   "ok",
			"instance": instance,
			"zone":     zone,
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
