// ml-analyzer is an offline module that periodically polls Prometheus
// for Envoy upstream-cluster metrics (p99 latency, RPS, error-rate) per
// (service, zone) pair, then forwards aggregated metrics to ml-inference
// (CatBoost model) to compute zone weights. Falls back to a local
// statistical heuristic when ml-inference is unavailable.
// The resulting weights + per-service rate-limit recommendations are
// published as JSON via HTTP.
//
// Endpoints:
//
//	GET /recommendations  Current snapshot of weights + rate_limits (JSON contract locked).
//	GET /healthz          200 if Prometheus has been polled successfully at least once.
//	GET /metrics          Prometheus text exposition of self-metrics.
//
// The recommendations are pure suggestions — applying them is the job of
// Control Plane.
//
// Usage:
//
//	ml-analyzer [flags]
//
// Flags:
//
//	-prometheus     Prometheus base URL (env: PROMETHEUS_URL, default: http://prometheus:9090)
//	-interval       Recommendation refresh interval (default: 60s)
//	-listen         HTTP listen address (default: :9200)
//	-sample-window  PromQL aggregation window (default: 5m)
package main

import (
	"context"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Service list is fixed by the project topology (5 services × 2 zones).
var services = []string{"service-a", "service-b", "service-c", "service-d", "service-e"}
var zones = []string{"zone1", "zone2"}

// clusterRE captures (service, zone) from an Envoy cluster label such as
// "service-a-zone1-cluster". Groups 1 and 2 yield the canonical hyphenated
// service name and zone tag — no underscore↔hyphen conversion.
var clusterRE = regexp.MustCompile(`^(service-[a-e])-(zone[12])-cluster$`)

// ZoneWeights is the weight pair for a single service.
type ZoneWeights struct {
	Zone1 float64 `json:"zone1"`
	Zone2 float64 `json:"zone2"`
}

// RateLimit is the per-service rate limit recommendation.
type RateLimit struct {
	RPS float64 `json:"rps"`
}

// Recommendations is the JSON wire contract published on GET /recommendations.
// Key names — "updated_at", "weights", "rate_limits", "zone1", "zone2", "rps"
// — are consumed verbatim by Control Plane.
type Recommendations struct {
	UpdatedAt  string                 `json:"updated_at"`
	Weights    map[string]ZoneWeights `json:"weights"`
	RateLimits map[string]RateLimit   `json:"rate_limits"`
}

// metricsState aggregates ml-analyzer self-metrics exposed at /metrics.
type metricsState struct {
	recommendationsGenerated atomic.Uint64
	lastRecommendationUnix   atomic.Int64
	queryErrors              sync.Map // query name (string) → *atomic.Uint64
	mlInferenceCalls         sync.Map // result ("ok"|"error") → *atomic.Uint64
}

func (m *metricsState) incQueryError(query string) {
	v, _ := m.queryErrors.LoadOrStore(query, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

func (m *metricsState) incMLInference(result string) {
	v, _ := m.mlInferenceCalls.LoadOrStore(result, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

func (m *metricsState) writeProm(w io.Writer) {
	fmt.Fprintln(w, "# HELP ml_recommendations_generated_total Total number of recommendation snapshots successfully generated.")
	fmt.Fprintln(w, "# TYPE ml_recommendations_generated_total counter")
	fmt.Fprintf(w, "ml_recommendations_generated_total %d\n", m.recommendationsGenerated.Load())

	fmt.Fprintln(w, "# HELP ml_last_recommendation_timestamp_seconds Unix timestamp (seconds) of the most recent successful recommendation snapshot.")
	fmt.Fprintln(w, "# TYPE ml_last_recommendation_timestamp_seconds gauge")
	fmt.Fprintf(w, "ml_last_recommendation_timestamp_seconds %d\n", m.lastRecommendationUnix.Load())

	fmt.Fprintln(w, "# HELP ml_prometheus_query_errors_total Number of failed Prometheus queries by query name.")
	fmt.Fprintln(w, "# TYPE ml_prometheus_query_errors_total counter")

	// Always print every known query label so target stays consistent even
	// before the first error happens.
	queries := []string{"p99", "rps", "errors"}
	for _, q := range queries {
		var n uint64
		if v, ok := m.queryErrors.Load(q); ok {
			n = v.(*atomic.Uint64).Load()
		}
		fmt.Fprintf(w, "ml_prometheus_query_errors_total{query=%q} %d\n", q, n)
	}

	fmt.Fprintln(w, "# HELP ml_inference_calls_total Number of calls to ml-inference service by result.")
	fmt.Fprintln(w, "# TYPE ml_inference_calls_total counter")
	for _, res := range []string{"ok", "error"} {
		var n uint64
		if v, ok := m.mlInferenceCalls.Load(res); ok {
			n = v.(*atomic.Uint64).Load()
		}
		fmt.Fprintf(w, "ml_inference_calls_total{result=%q} %d\n", res, n)
	}
}

// analyzer is the long-running periodic Prometheus poller.
type analyzer struct {
	promURL      string
	httpClient   *http.Client
	sampleWindow time.Duration
	metrics      *metricsState
	mlInferenceURL string // empty = disabled

	mu                  sync.RWMutex
	current             *Recommendations
	lastSuccessUnix     atomic.Int64
	lastSuccessRecorded atomic.Bool
}

func newAnalyzer(promURL, mlInferenceURL string, sampleWindow time.Duration, metrics *metricsState) *analyzer {
	return &analyzer{
		promURL:        promURL,
		mlInferenceURL: mlInferenceURL,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		sampleWindow:   sampleWindow,
		metrics:        metrics,
	}
}

// snapshot returns the most recent published recommendations, or nil if none yet.
func (a *analyzer) snapshot() *Recommendations {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.current
}

// setSnapshot atomically replaces the published recommendations.
func (a *analyzer) setSnapshot(r *Recommendations) {
	a.mu.Lock()
	a.current = r
	a.mu.Unlock()
}

// promResult is the per-cluster scalar value from a Prometheus instant query.
type promResult map[string]float64 // envoy_cluster_name → value

// runOnce polls Prometheus once and updates the in-memory snapshot if successful.
func (a *analyzer) runOnce(ctx context.Context) error {
	window := a.sampleWindow.String()

	p99Query := fmt.Sprintf(
		`histogram_quantile(0.99, sum by (envoy_cluster_name, le) `+
			`(rate(envoy_cluster_upstream_rq_time_bucket{envoy_cluster_name=~"service-[a-e]-zone[12]-cluster"}[%s])))`,
		window,
	)
	rpsQuery := fmt.Sprintf(
		`sum by (envoy_cluster_name) `+
			`(rate(envoy_cluster_upstream_rq_total{envoy_cluster_name=~"service-[a-e]-zone[12]-cluster"}[%s]))`,
		window,
	)
	errQuery := fmt.Sprintf(
		`sum by (envoy_cluster_name) `+
			`(rate(envoy_cluster_upstream_rq_xx{envoy_response_code_class="5", envoy_cluster_name=~"service-[a-e]-zone[12]-cluster"}[%s]))`,
		window,
	)

	p99, p99Err := a.queryByCluster(ctx, p99Query)
	if p99Err != nil {
		a.metrics.incQueryError("p99")
	}
	rps, rpsErr := a.queryByCluster(ctx, rpsQuery)
	if rpsErr != nil {
		a.metrics.incQueryError("rps")
	}
	errs, errsErr := a.queryByCluster(ctx, errQuery)
	if errsErr != nil {
		a.metrics.incQueryError("errors")
	}

	// If every query failed, surface a single error so the caller can decide
	// whether to keep the last known snapshot or report degradation.
	if p99Err != nil && rpsErr != nil && errsErr != nil {
		return fmt.Errorf("all prometheus queries failed: p99=%v rps=%v errors=%v", p99Err, rpsErr, errsErr)
	}

	rec := buildRecommendations(p99, rps, errs)

	// Attempt to override heuristic weights with ML-inference prediction.
	// callMLInference aggregates global metrics from the raw Prometheus maps
	// and POSTs to ml-inference. On success the returned split replaces every
	// service's weight; on any failure we keep the heuristic weights.
	if a.mlInferenceURL != "" {
		if mlZone1, ok := a.callMLInference(ctx, p99, rps, errs); ok {
			a.metrics.incMLInference("ok")
			mlZone2 := round3(1.0 - mlZone1)
			mlZone1 = round3(mlZone1)
			for svc, w := range rec.Weights {
				_ = w
				rec.Weights[svc] = ZoneWeights{Zone1: mlZone1, Zone2: mlZone2}
			}
			slog.Info("ml-inference weights applied",
				slog.Float64("zone1", mlZone1),
				slog.Float64("zone2", mlZone2),
			)
		} else {
			a.metrics.incMLInference("error")
			slog.Warn("ml-inference unavailable, using heuristic weights")
		}
	}

	a.setSnapshot(rec)
	now := time.Now().Unix()
	a.lastSuccessUnix.Store(now)
	a.lastSuccessRecorded.Store(true)
	a.metrics.recommendationsGenerated.Add(1)
	a.metrics.lastRecommendationUnix.Store(now)
	return nil
}

// callMLInference aggregates the raw Prometheus metric maps into a single
// MetricsRequest payload and POSTs it to ml-inference /api/v1/predict.
// Returns (zone1Weight, true) on success; (0, false) on any error so the
// caller can fall back to heuristic weights without logging noise.
func (a *analyzer) callMLInference(ctx context.Context, p99, rps, errs promResult) (float64, bool) {
	type metricsRequest struct {
		GlobalRPS              float64  `json:"global_rps"`
		Zone1RPS               float64  `json:"zone1_rps"`
		Zone2RPS               float64  `json:"zone2_rps"`
		GlobalErrorRatePct     float64  `json:"global_error_rate_pct"`
		Zone1ErrorRatePct      *float64 `json:"zone1_error_rate_pct,omitempty"`
		Zone2ErrorRatePct      *float64 `json:"zone2_error_rate_pct,omitempty"`
		Zone1LatencyP99Ms      *float64 `json:"zone1_latency_p99_ms,omitempty"`
		Zone2LatencyP99Ms      *float64 `json:"zone2_latency_p99_ms,omitempty"`
		Zone1ActiveCx          float64  `json:"zone1_active_cx"`
		Zone2ActiveCx          float64  `json:"zone2_active_cx"`
		Zone1Ejections         float64  `json:"zone1_ejections"`
		Zone2Ejections         float64  `json:"zone2_ejections"`
		GeoClusterEjections    float64  `json:"geo_cluster_ejections"`
		GlobalDownstreamCxActive float64 `json:"global_downstream_cx_active"`
		Zone1UpstreamRqRetry   float64  `json:"zone1_upstream_rq_retry"`
		Zone2UpstreamRqRetry   float64  `json:"zone2_upstream_rq_retry"`
		Zone1UpstreamRqPending float64  `json:"zone1_upstream_rq_pending_total"`
		Zone2UpstreamRqPending float64  `json:"zone2_upstream_rq_pending_total"`
	}

	// Aggregate across all services: sum RPS per zone, average p99 per zone.
	var z1RPS, z2RPS, z1Err, z2Err float64
	var z1P99, z2P99 float64
	z1P99Count, z2P99Count := 0, 0
	for cluster, v := range rps {
		m := clusterRE.FindStringSubmatch(cluster)
		if m == nil {
			continue
		}
		if m[2] == "zone1" {
			z1RPS += v
		} else {
			z2RPS += v
		}
	}
	for cluster, v := range errs {
		m := clusterRE.FindStringSubmatch(cluster)
		if m == nil {
			continue
		}
		if m[2] == "zone1" {
			z1Err += v
		} else {
			z2Err += v
		}
	}
	for cluster, v := range p99 {
		m := clusterRE.FindStringSubmatch(cluster)
		if m == nil {
			continue
		}
		if m[2] == "zone1" {
			z1P99 += v
			z1P99Count++
		} else {
			z2P99 += v
			z2P99Count++
		}
	}
	if z1P99Count > 0 {
		z1P99 /= float64(z1P99Count)
	}
	if z2P99Count > 0 {
		z2P99 /= float64(z2P99Count)
	}

	globalRPS := z1RPS + z2RPS
	var z1ErrPct, z2ErrPct *float64
	if z1RPS > 0 {
		v := (z1Err / z1RPS) * 100.0
		z1ErrPct = &v
	}
	if z2RPS > 0 {
		v := (z2Err / z2RPS) * 100.0
		z2ErrPct = &v
	}
	globalErrPct := 0.0
	if globalRPS > 0 {
		globalErrPct = ((z1Err + z2Err) / globalRPS) * 100.0
	}
	var z1P99Ptr, z2P99Ptr *float64
	if z1P99Count > 0 {
		z1P99Ptr = &z1P99
	}
	if z2P99Count > 0 {
		z2P99Ptr = &z2P99
	}

	payload := metricsRequest{
		GlobalRPS:          globalRPS,
		Zone1RPS:           z1RPS,
		Zone2RPS:           z2RPS,
		GlobalErrorRatePct: globalErrPct,
		Zone1ErrorRatePct:  z1ErrPct,
		Zone2ErrorRatePct:  z2ErrPct,
		Zone1LatencyP99Ms:  z1P99Ptr,
		Zone2LatencyP99Ms:  z2P99Ptr,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, false
	}

	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		a.mlInferenceURL+"/api/v1/predict",
		bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, false
	}

	var result struct {
		TrafficSplitZone1 float64 `json:"traffic_split_zone1"`
		TrafficSplitZone2 float64 `json:"traffic_split_zone2"`
		Confidence        float64 `json:"confidence"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, false
	}
	if result.TrafficSplitZone1 <= 0 && result.TrafficSplitZone2 <= 0 {
		return 0, false
	}
	return result.TrafficSplitZone1, true
}

// queryByCluster runs a PromQL instant query and groups the results by
// envoy_cluster_name. NaN / Inf values are filtered out.
func (a *analyzer) queryByCluster(ctx context.Context, query string) (promResult, error) {
	reqURL := a.promURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned status %d", resp.StatusCode)
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string  `json:"metric"`
				Value  [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus query status=%s", body.Status)
	}

	out := make(promResult, len(body.Data.Result))
	for _, r := range body.Data.Result {
		name := r.Metric["envoy_cluster_name"]
		if name == "" {
			continue
		}
		var s string
		if err := json.Unmarshal(r.Value[1], &s); err != nil {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[name] = v
	}
	return out, nil
}

// reshape converts a flat envoy_cluster_name → value map into the nested
// service → zone → value map needed for per-service aggregation. Cluster
// names that don't match the canonical pattern are silently dropped — they
// are not part of the locked service catalogue.
func reshape(m promResult) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(services))
	for cluster, v := range m {
		match := clusterRE.FindStringSubmatch(cluster)
		if match == nil {
			continue
		}
		svc, zone := match[1], match[2]
		bySvc, ok := out[svc]
		if !ok {
			bySvc = make(map[string]float64, len(zones))
			out[svc] = bySvc
		}
		bySvc[zone] = v
	}
	return out
}

// buildRecommendations consumes the three per-cluster metric maps and produces
// the canonical Recommendations snapshot. All 5 services are always present,
// every service has exactly zone1+zone2 weight keys, and weights sum to 1.0.
func buildRecommendations(p99, rps, errs promResult) *Recommendations {
	p99By := reshape(p99)
	rpsBy := reshape(rps)
	errBy := reshape(errs)

	rec := &Recommendations{
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
		Weights:    make(map[string]ZoneWeights, len(services)),
		RateLimits: make(map[string]RateLimit, len(services)),
	}

	for _, svc := range services {
		// Per-(service, zone) score and per-(service, zone) RPS.
		scores := make(map[string]float64, len(zones))
		zoneRPS := make(map[string]float64, len(zones))
		var anyData bool

		for _, zone := range zones {
			hasP99 := false
			p99ms := 0.0
			if v, ok := p99By[svc][zone]; ok {
				// envoy_cluster_upstream_rq_time_bucket buckets are in ms (Envoy default).
				p99ms = v
				hasP99 = true
			}
			errRate := 0.0
			r := rpsBy[svc][zone]
			zoneRPS[zone] = r
			if e, ok := errBy[svc][zone]; ok && r > 0 {
				errRate = e / r
				if errRate > 1.0 {
					errRate = 1.0
				}
			}
			if hasP99 || r > 0 {
				anyData = true
			}
			// score = 1 / (1 + p99_ms/100) * (1 - error_rate)
			score := 1.0 / (1.0 + p99ms/100.0) * (1.0 - errRate)
			if score < 0 {
				score = 0
			}
			scores[zone] = score
		}

		// Weights: normalise so zone1 + zone2 == 1.0. Fallback to even split
		// when both scores are zero / no data is present.
		var w ZoneWeights
		sum := scores["zone1"] + scores["zone2"]
		if !anyData || sum <= 0 {
			w = ZoneWeights{Zone1: 0.5, Zone2: 0.5}
		} else {
			z1 := scores["zone1"] / sum
			z2 := scores["zone2"] / sum
			// Renormalise to guarantee |z1+z2 - 1.0| ≤ 0.01 even with float error.
			total := z1 + z2
			if total > 0 {
				z1 /= total
				z2 = 1.0 - z1
			}
			w = ZoneWeights{Zone1: round3(z1), Zone2: round3(z2)}
			// Final correction after rounding so the sum is exactly 1.0 to 3 dp.
			if d := 1.0 - (w.Zone1 + w.Zone2); math.Abs(d) > 1e-9 {
				w.Zone2 = round3(w.Zone2 + d)
			}
		}
		rec.Weights[svc] = w

		// Rate limit: 1.5 × max(zone_rps); fallback 100 when sample RPS < 1.
		sample := math.Max(zoneRPS["zone1"], zoneRPS["zone2"])
		var rpsRec float64
		if sample < 1.0 {
			rpsRec = 100
		} else {
			rpsRec = math.Round(sample * 1.5)
		}
		if rpsRec <= 0 {
			rpsRec = 100
		}
		rec.RateLimits[svc] = RateLimit{RPS: rpsRec}
	}
	return rec
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func main() {
	prom := flag.String("prometheus", "", "Prometheus base URL (env: PROMETHEUS_URL)")
	interval := flag.Duration("interval", 60*time.Second, "Recommendation refresh interval")
	listen := flag.String("listen", ":9200", "HTTP listen address")
	sampleWindow := flag.Duration("sample-window", 5*time.Minute, "PromQL aggregation window")
	mlInf := flag.String("ml-inference-url", "", "ml-inference base URL (env: ML_INFERENCE_URL, e.g. http://ml-inference:8000)")
	flag.Parse()

	// Flag → env → default precedence (avoid putting env into flag.String's
	// default value, which obscures which source provided it).
	if *prom == "" {
		*prom = os.Getenv("PROMETHEUS_URL")
	}
	if *prom == "" {
		*prom = "http://prometheus:9090"
	}
	if *mlInf == "" {
		*mlInf = os.Getenv("ML_INFERENCE_URL")
	}
	// No hard default: if unset ml-inference is simply not consulted.

	// Operational logs → stderr (JSON).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if *interval <= 0 {
		slog.Error("interval must be > 0", slog.Duration("interval", *interval))
		os.Exit(1)
	}
	if *sampleWindow <= 0 {
		slog.Error("sample-window must be > 0", slog.Duration("sample-window", *sampleWindow))
		os.Exit(1)
	}

	metrics := &metricsState{}
	an := newAnalyzer(*prom, *mlInf, *sampleWindow, metrics)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /recommendations", func(w http.ResponseWriter, r *http.Request) {
		snap := an.snapshot()
		w.Header().Set("Content-Type", "application/json")
		if snap == nil {
			// No successful poll yet — graceful 503 with a small JSON body.
			w.WriteHeader(http.StatusServiceUnavailable)
			body := map[string]string{
				"error":        "no recommendations yet",
				"last_success": "",
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			slog.Warn("encode recommendations failed", slog.Any("err", err))
		}
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !an.lastSuccessRecorded.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":        "prometheus unreachable",
				"last_success": "",
			})
			return
		}
		last := time.Unix(an.lastSuccessUnix.Load(), 0).UTC()
		// Consider analyzer unhealthy if the last successful poll is older
		// than 3 × interval (gives room for a couple of transient failures).
		threshold := 3 * (*interval)
		if time.Since(last) > threshold {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":        "prometheus unreachable",
				"last_success": last.Format(time.RFC3339),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":       "ok",
			"last_success": last.Format(time.RFC3339),
		})
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		metrics.writeProm(w)
	})

	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Periodic poller. Runs once on start so /recommendations isn't empty
	// for an entire `-interval` if Prometheus is already up.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runWithLog := func() {
			pollCtx, pollCancel := context.WithTimeout(ctx, 30*time.Second)
			defer pollCancel()
			if err := an.runOnce(pollCtx); err != nil {
				slog.Warn("prometheus poll failed", slog.Any("err", err))
				return
			}
			slog.Info("recommendations refreshed",
				slog.Int("services", len(services)),
				slog.String("sample_window", an.sampleWindow.String()),
			)
		}
		runWithLog()
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runWithLog()
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("ml-analyzer listening",
			slog.String("addr", *listen),
			slog.String("prometheus_url", *prom),
			slog.Duration("interval", *interval),
			slog.Duration("sample_window", *sampleWindow),
			slog.String("ml_inference_url", *mlInf),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", slog.Any("err", err))
			cancel()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("server shutdown error", slog.Any("err", err))
	}
	wg.Wait()
}

