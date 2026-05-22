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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/geo-distributed-gateway/sdk/config"
)

// Snapshot is the in-memory routing-policy snapshot. Field names and JSON
// tags form the wire contract: /config response and the Redis publisher
// payload both consume them verbatim.
type Snapshot struct {
	Version    int                            `json:"version"`
	UpdatedAt  time.Time                      `json:"updated_at"`
	Weights    map[string]map[string]float64  `json:"weights"`
	RateLimits map[string]RateLimit           `json:"rate_limits"`
}

// RateLimit is the per-service rate limit. Field name is part of the wire contract.
type RateLimit struct {
	RPS int `json:"rps"`
}

// recommendations mirrors the JSON wire contract published by ml-analyzer.
// Decoded into our Snapshot in pullOnce.
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

	// Redis publish counters partitioned by (service, result).
	// We don't know services up-front, so guard with a mutex; cardinality is
	// small (5 services × 2 outcomes), reads happen only on /metrics scrape.
	pubMu       sync.Mutex
	pubSuccess  map[string]int64
	pubErrors   map[string]int64
}

func newState() *state {
	s := &state{
		pubSuccess: map[string]int64{},
		pubErrors:  map[string]int64{},
	}
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

// recordPublish updates the per-service Redis-publish counter. Cardinality
// is small (one entry per service), so a single mutex is fine.
func (s *state) recordPublish(service string, ok bool) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if ok {
		s.pubSuccess[service]++
	} else {
		s.pubErrors[service]++
	}
}

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

	// Redis pub/sub publisher: best-effort. An empty/unparseable REDIS_URL or
	// a connect failure must NOT take the service down — Control Plane keeps
	// serving /config, /healthz, /metrics. T7 acceptance: when Redis is down,
	// stub services keep working; on Redis recovery, publishes resume on the
	// next pull cycle. A nil publisher signals "skip publish entirely".
	var publisher *redisPublisher
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		p, err := newRedisPublisher(redisURL)
		if err != nil {
			slog.Warn("redis publisher disabled", slog.String("url", redisURL), slog.Any("err", err))
		} else {
			publisher = p
			defer publisher.Close()
			slog.Info("redis publisher ready", slog.String("url", redisURL))
		}
	} else {
		slog.Warn("REDIS_URL is empty; routing hints will not be broadcast")
	}

	go pollLoop(ctx, st, *mlURL, *pollInterval, publisher)

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
//
// On every successful pull, the loop also broadcasts the freshly stored
// snapshot to Redis Pub/Sub (T7). The first successful pull after restart
// always publishes (no previous state to compare); subsequent pulls publish
// only when (weights, rate_limits) actually changed — see publishIfChanged.
func pollLoop(ctx context.Context, st *state, mlURL string, interval time.Duration, publisher *redisPublisher) {
	client := &http.Client{Timeout: 10 * time.Second}

	// firstPublishDone flips after the first publish broadcast (success or
	// partial). Until then we always publish, regardless of content equality
	// with the empty initial snapshot. This guarantees acceptance "5 messages
	// after restart" even if ML happens to return the same content as the
	// previous run.
	var firstPublishDone bool
	// prevPublished tracks the last successfully attempted-to-publish state
	// for content comparison on subsequent pulls. We compare the actual
	// payload fields, not the Version, so we don't republish on a version
	// bump that re-uses the same content.
	var prevPublished Snapshot

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

		if publisher == nil {
			// Routing-hints fanout is disabled; nothing else to do.
			return
		}

		// Always-publish on the first successful pull after process start;
		// otherwise compare new vs. last-published content and skip if equal.
		current := st.get()
		shouldPublish := !firstPublishDone ||
			!reflect.DeepEqual(prevPublished.Weights, current.Weights) ||
			!reflect.DeepEqual(prevPublished.RateLimits, current.RateLimits)
		if !shouldPublish {
			return
		}
		publisher.PublishSnapshot(ctx, st, current)
		prevPublished = current
		firstPublishDone = true
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
// prometheus/client_golang to keep this binary's dep tree empty.
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

		// Redis publishes per (service, result). We render success+error rows
		// for every service that has a non-zero counter on either dimension —
		// Prometheus tolerates absent series for new labels but giving both
		// outcomes per service keeps alert rules simple (no NaN math).
		st.pubMu.Lock()
		services := make(map[string]struct{}, len(st.pubSuccess)+len(st.pubErrors))
		for svc := range st.pubSuccess {
			services[svc] = struct{}{}
		}
		for svc := range st.pubErrors {
			services[svc] = struct{}{}
		}
		ordered := make([]string, 0, len(services))
		for svc := range services {
			ordered = append(ordered, svc)
		}
		sort.Strings(ordered) // deterministic ordering for scrapers/tests
		successCopy := make(map[string]int64, len(st.pubSuccess))
		errorCopy := make(map[string]int64, len(st.pubErrors))
		for k, v := range st.pubSuccess {
			successCopy[k] = v
		}
		for k, v := range st.pubErrors {
			errorCopy[k] = v
		}
		st.pubMu.Unlock()

		fmt.Fprintln(w, "# HELP control_plane_redis_publishes_total Total Redis pub/sub publish attempts partitioned by service and result.")
		fmt.Fprintln(w, "# TYPE control_plane_redis_publishes_total counter")
		for _, svc := range ordered {
			fmt.Fprintf(w, "control_plane_redis_publishes_total{service=%q,result=\"success\"} %d\n", svc, successCopy[svc])
			fmt.Fprintf(w, "control_plane_redis_publishes_total{service=%q,result=\"error\"} %d\n", svc, errorCopy[svc])
		}
	}
}

// redisPublisher fans out per-service RoutingHints to the
// "routing:<service>" channels. It is a thin wrapper around go-redis with a
// known-services list so a single snapshot turns into N publishes.
type redisPublisher struct {
	client *redis.Client
}

func newRedisPublisher(url string) (*redisPublisher, error) {
	opts, err := parseRedisURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}
	// We deliberately skip an up-front Ping here: Redis may not be ready when
	// control-plane boots, but go-redis will auto-reconnect on every Publish
	// when the server comes back. This matches T7 acceptance scenario "при
	// `docker start redis` через 30s — publish возобновляется на следующем
	// pull-цикле" without bespoke retry logic.
	client := redis.NewClient(opts)
	return &redisPublisher{client: client}, nil
}

// PublishSnapshot publishes one RoutingHints per service mentioned in the
// snapshot. Each publish failure is recorded as an error metric but does
// not stop the loop — partial fan-out is preferred over all-or-nothing.
func (p *redisPublisher) PublishSnapshot(ctx context.Context, st *state, snap Snapshot) {
	// Union the keys from weights and rate_limits so a service mentioned in
	// only one of them still gets a publish (defensive — ml-analyzer is
	// expected to populate both, but T7 should not silently drop a service
	// that's present in one map).
	services := make(map[string]struct{}, len(snap.Weights)+len(snap.RateLimits))
	for svc := range snap.Weights {
		services[svc] = struct{}{}
	}
	for svc := range snap.RateLimits {
		services[svc] = struct{}{}
	}
	for svc := range services {
		hint := config.RoutingHints{
			Service:    svc,
			Version:    snap.Version,
			UpdatedAt:  snap.UpdatedAt,
			Weights:    snap.Weights[svc],
			RateLimits: map[string]int{"rps": snap.RateLimits[svc].RPS},
		}
		body, err := json.Marshal(hint)
		if err != nil {
			st.recordPublish(svc, false)
			slog.Warn("redis publish: marshal failed", slog.String("service", svc), slog.Any("err", err))
			continue
		}
		pubCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = p.client.Publish(pubCtx, "routing:"+svc, body).Err()
		cancel()
		if err != nil {
			st.recordPublish(svc, false)
			slog.Warn("redis publish failed", slog.String("service", svc), slog.Any("err", err))
			continue
		}
		st.recordPublish(svc, true)
		slog.Info("redis publish ok",
			slog.String("service", svc),
			slog.Int("version", hint.Version),
		)
	}
}

func (p *redisPublisher) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	return p.client.Close()
}

// parseRedisURL accepts both a full redis-go URL and a bare "host:port"
// short form (e.g. "redis:6379") that docker-compose conventionally puts
// in REDIS_URL. Mirrors sdk/config.parseRedisURL so this binary doesn't
// have to import the SDK just for URL parsing — keeps the layering clean
// (control-plane uses the redis client directly, SDK consumes hints).
func parseRedisURL(s string) (*redis.Options, error) {
	if s == "" {
		return nil, fmt.Errorf("empty redis URL")
	}
	if strings.HasPrefix(s, "redis://") || strings.HasPrefix(s, "rediss://") {
		return redis.ParseURL(s)
	}
	return redis.ParseURL("redis://" + s)
}
