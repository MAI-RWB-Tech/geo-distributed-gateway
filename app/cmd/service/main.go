package main

import (
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
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

	col := telemetry.New("stub-service", instance, zone, nil)
	col.Start()
	defer col.Stop()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
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

	addr := ":8080"
	slog.Info("Starting stub service", slog.String("addr", addr), slog.String("instance", instance))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
