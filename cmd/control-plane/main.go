// control-plane periodically polls the ML analyzer for routing recommendations
// and publishes them via HTTP for consumption by zone-Envoy (Lua score plugin,
// T6) and stub services (Redis Pub/Sub, T7).
//
// The service owns the in-memory "truth" about current weights and rate limits.
// It does not apply config to Envoy directly — application is T6/T7.
//
// Usage:
//
//	control-plane [flags]
//
// Flags:
//
//	-ml-url          ML analyzer base URL (env: ML_URL, default: http://ml-analyzer:9200)
//	-poll-interval   How often to pull /recommendations (default: 30s)
//	-listen          HTTP listen address (default: :9300)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// Snapshot is the in-memory routing-policy snapshot.
// Field names are LOCKED (see plan.md T4 step 3): T7 consumes
// snapshot.RateLimits[svc].RPS verbatim, and the JSON tags pin the
// /config wire contract.
type Snapshot struct {
	Version    int                            `json:"version"`
	UpdatedAt  time.Time                      `json:"updated_at"`
	Weights    map[string]map[string]float64  `json:"weights"`
	RateLimits map[string]RateLimit           `json:"rate_limits"`
}

// RateLimit is the per-service rate limit. Field name LOCKED.
type RateLimit struct {
	RPS int `json:"rps"`
}

// recommendations mirrors the JSON wire contract published by ml-analyzer
// (see plan.md T3 step 6). Decoded into our Snapshot in pullOnce.
type recommendations struct {
	UpdatedAt  string                        `json:"updated_at"`
	Weights    map[string]map[string]float64 `json:"weights"`
	RateLimits map[string]rateLimitDTO       `json:"rate_limits"`
}

type rateLimitDTO struct {
	RPS int `json:"rps"`
}

// state holds the live snapshot plus self-metrics. Reads are lock-free via
// atomic.Value; writes happen only inside the polling goroutine, so the
// store is single-writer.
type state struct {
	snap        atomic.Value // Snapshot
	lastSuccess atomic.Int64 // unix nanos of last successful pull; 0 means "never"
	successes   atomic.Int64
	errors      atomic.Int64
}

func newState() *state {
	s := &state{}
	// Initial snapshot is empty but valid — graceful degradation per plan T4 step 4.
	s.snap.Store(Snapshot{
		Version:    0,
		UpdatedAt:  time.Time{},
		Weights:    map[string]map[string]float64{},
		RateLimits: map[string]RateLimit{},
	})
	return s
}

func (s *state) get() Snapshot { return s.snap.Load().(Snapshot) }

func main() {
	mlURL := flag.String("ml-url", "", "ML analyzer base URL (env: ML_URL)")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "How often to pull /recommendations")
	listen := flag.String("listen", ":9300", "HTTP listen address")
	healthMaxAgeFactor := flag.Float64("health-max-age-factor", 2.5,
		"Mark /healthz 503 when seconds-since-last-successful-pull exceeds factor*poll-interval")
	flag.Parse()

	// flag → env → default precedence.
	if *mlURL == "" {
		*mlURL = os.Getenv("ML_URL")
	}
	if *mlURL == "" {
		*mlURL = "http://ml-analyzer:9200"
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	st := newState()

	healthMaxAge := time.Duration(float64(*pollInterval) * *healthMaxAgeFactor)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", handleConfig(st))
	mux.HandleFunc("GET /config/weights/{service}", handleWeights(st))
	mux.HandleFunc("GET /healthz", handleHealthz(st, healthMaxAge))
	mux.HandleFunc("GET /metrics", handleMetrics(st))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go pollLoop(ctx, st, *mlURL, *pollInterval)

	go func() {
		slog.Info("control-plane starting",
			slog.String("listen", *listen),
			slog.String("ml_url", *mlURL),
			slog.Duration("poll_interval", *pollInterval),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", slog.Any("err", err))
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("control-plane shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", slog.Any("err", err))
	}
}

// pollLoop fetches recommendations from ML on every tick and updates state.
// It runs the first pull immediately so /config has fresh data well before
// the first tick (default 30s) elapses, but failures during this first pull
// are non-fatal: the empty-snapshot initial state remains in place.
func pollLoop(ctx context.Context, st *state, mlURL string, interval time.Duration) {
	client := &http.Client{Timeout: 10 * time.Second}

	pull := func() {
		if err := pullOnce(ctx, client, mlURL, st); err != nil {
			st.errors.Add(1)
			slog.Warn("ml pull failed",
				slog.String("ml_url", mlURL),
				slog.Any("err", err),
			)
			return
		}
		st.successes.Add(1)
		st.lastSuccess.Store(time.Now().UnixNano())
	}

	pull() // immediate first pull

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pull()
		}
	}
}

// pullOnce performs a single GET <mlURL>/recommendations and, on success,
// atomically replaces the snapshot. Version is incremented only when the
// (weights, rate_limits) tuple differs from the previous successful snapshot.
func pullOnce(ctx context.Context, client *http.Client, mlURL string, st *state) error {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, mlURL+"/recommendations", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a small chunk for diagnostics; cap to avoid runaway memory
		// if ML misbehaves.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var rec recommendations
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	// Normalize: convert ml-analyzer's rate_limits sub-struct into our
	// RateLimit type so the wire and storage formats match.
	rl := make(map[string]RateLimit, len(rec.RateLimits))
	for svc, v := range rec.RateLimits {
		rl[svc] = RateLimit{RPS: v.RPS}
	}

	// Defensive copy of weights to ensure no aliasing between
	// the decoded body and the stored snapshot.
	w := make(map[string]map[string]float64, len(rec.Weights))
	for svc, zw := range rec.Weights {
		inner := make(map[string]float64, len(zw))
		for z, v := range zw {
			inner[z] = v
		}
		w[svc] = inner
	}

	prev := st.get()
	next := Snapshot{
		UpdatedAt:  time.Now().UTC(),
		Weights:    w,
		RateLimits: rl,
	}

	// Version increment policy: bump only when (weights, rate_limits) changed.
	// reflect.DeepEqual handles nested maps; performance is not a concern at
	// 30s cadence with 5 services × 2 zones.
	if reflect.DeepEqual(prev.Weights, next.Weights) && reflect.DeepEqual(prev.RateLimits, next.RateLimits) {
		next.Version = prev.Version
		if next.Version == 0 {
			// First successful pull: bump from 0 to 1 even if the payload happens
			// to deep-equal the empty initial state. This makes /healthz flip
			// to 200 promptly and matches the acceptance criterion "version ≥ 1".
			next.Version = 1
		}
	} else {
		next.Version = prev.Version + 1
	}

	st.snap.Store(next)
	slog.Info("ml pull ok",
		slog.Int("version", next.Version),
		slog.Int("services", len(next.Weights)),
	)
	return nil
}

func handleConfig(st *state) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(st.get())
	}
}

func handleWeights(st *state) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := r.PathValue("service")
		snap := st.get()
		// Treat empty Weights (no successful pull yet) as 404 for any service
		// per plan T4 step 5 — Lua filter (T6) should fall back to default
		// behavior in that case.
		if len(snap.Weights) == 0 {
			http.NotFound(w, r)
			return
		}
		zw, ok := snap.Weights[svc]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(zw)
	}
}

// handleHealthz returns 200 when the last successful ML pull happened recently
// enough (within maxAge), otherwise 503. This combines two concerns:
//  1. "Never had a successful pull" — initial state with version=0 → 503
//  2. "ML went down after working" — last success is too old → 503
//
// Plan T4 step 5 specifies "200 if version > 0", but the same plan's acceptance
// scenario A requires 503 after ML is stopped (when version is still > 0).
// Resolving the conflict in favor of freshness gives a real liveness signal
// (see DL-T4-002).
func handleHealthz(st *state, maxAge time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := st.get()
		w.Header().Set("Content-Type", "application/json")
		lastNS := st.lastSuccess.Load()
		fresh := lastNS > 0 && time.Since(time.Unix(0, lastNS)) <= maxAge
		if snap.Version > 0 && fresh {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"version": snap.Version,
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		lastPull := ""
		if lastNS > 0 {
			lastPull = time.Unix(0, lastNS).UTC().Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":     "ml unreachable",
			"last_pull": lastPull,
		})
	}
}

// handleMetrics serves a minimal Prometheus text-format exposition.
// We intentionally hand-roll the format instead of pulling in
// prometheus/client_golang to keep this binary's dep tree empty (see DL-T4-001).
func handleMetrics(st *state) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := st.get()
		ageSec := 0.0
		if ns := st.lastSuccess.Load(); ns > 0 {
			ageSec = time.Since(time.Unix(0, ns)).Seconds()
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// One metric per HELP/TYPE block, per Prometheus exposition format.
		fmt.Fprintln(w, "# HELP control_plane_config_version Current config snapshot version (monotonic, increments when ML payload changes).")
		fmt.Fprintln(w, "# TYPE control_plane_config_version gauge")
		fmt.Fprintf(w, "control_plane_config_version %d\n", snap.Version)

		fmt.Fprintln(w, "# HELP control_plane_ml_pulls_total Total ML pull attempts partitioned by outcome.")
		fmt.Fprintln(w, "# TYPE control_plane_ml_pulls_total counter")
		fmt.Fprintf(w, "control_plane_ml_pulls_total{result=\"success\"} %d\n", st.successes.Load())
		fmt.Fprintf(w, "control_plane_ml_pulls_total{result=\"error\"} %d\n", st.errors.Load())

		fmt.Fprintln(w, "# HELP control_plane_age_seconds Seconds since the last successful ML pull. 0 means no successful pull yet.")
		fmt.Fprintln(w, "# TYPE control_plane_age_seconds gauge")
		// Use strconv for the float so we get a deterministic format without
		// locale-dependent quirks.
		fmt.Fprintf(w, "control_plane_age_seconds %s\n", strconv.FormatFloat(ageSec, 'f', 3, 64))
	}
}
