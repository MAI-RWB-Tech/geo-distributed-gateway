// failure-runner orchestrates failure injection scenarios against the geo-distributed gateway.
// It uses the container runtime CLI (podman/docker stop/start/pause/unpause)
// and queries Prometheus before and after each scenario to measure impact.
//
// Usage:
//
//	failure-runner [flags] <scenario>
//
// Scenarios:
//
//	zone1-full      Stop all containers in zone1 (full DC outage)
//	zone2-full      Stop all containers in zone2 (full DC outage)
//	zone1-partial   Stop half the containers in zone1 (partial degradation)
//	zone2-partial   Stop half the containers in zone2
//	zone1-pause     Pause zone1 app containers (network-level blackhole)
//	zone2-pause     Pause zone2 app containers
//
// After the scenario, hit Ctrl+C or wait for -restore-after to auto-restore.
//
// Flags:
//
//	-runtime        Container runtime: podman or docker (default: podman)
//	-prometheus     Prometheus base URL (default: http://localhost:9090)
//	-restore-after  Auto-restore containers after this duration (0 = manual, default: 30s)
//	-probe-url      Gateway URL to probe during scenario (default: http://localhost:10000/ping)
//	-probe-rps      Probe RPS during scenario (default: 20)
//	-zone           Zone containers: -zone name=c1,c2 (repeatable, any number of zones)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/geo-distributed-gateway/sdk/client"
	"github.com/geo-distributed-gateway/sdk/stats"
)

// zonesFlag is a repeatable -zone flag that builds a map of zone → containers.
// Each use has the form:  -zone name=container1,container2
type zonesFlag map[string][]string

func (z zonesFlag) String() string {
	keys := make([]string, 0, len(z))
	for k := range z {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(z))
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Join(z[k], ","))
	}
	return strings.Join(parts, "; ")
}

func (z zonesFlag) Set(s string) error {
	name, list, ok := strings.Cut(s, "=")
	if !ok || name == "" || list == "" {
		return fmt.Errorf("want zone=container1,container2, got %q", s)
	}
	z[name] = strings.Split(list, ",")
	return nil
}

type scenario struct {
	name        string
	description string
	zone        string
	partial     bool // only half the containers
	pause       bool // pause instead of stop
}

var scenarios = map[string]scenario{
	"zone1-full":    {"zone1-full", "Stop all zone1 app containers (full DC outage)", "zone1", false, false},
	"zone2-full":    {"zone2-full", "Stop all zone2 app containers (full DC outage)", "zone2", false, false},
	"zone1-partial": {"zone1-partial", "Stop half zone1 app containers (partial degradation)", "zone1", true, false},
	"zone2-partial": {"zone2-partial", "Stop half zone2 app containers (partial degradation)", "zone2", true, false},
	"zone1-pause":   {"zone1-pause", "Pause zone1 containers (simulates network blackhole)", "zone1", false, true},
	"zone2-pause":   {"zone2-pause", "Pause zone2 containers (simulates network blackhole)", "zone2", false, true},
}

func main() {
	runtime      := flag.String("runtime", "podman", "Container runtime: podman or docker")
	prometheusURL := flag.String("prometheus", "http://localhost:9090", "Prometheus base URL")
	restoreAfter := flag.Duration("restore-after", 30*time.Second, "Auto-restore after duration (0 = manual)")
	probeURL     := flag.String("probe-url", "http://localhost:10000/ping", "Gateway URL to probe during scenario")
	probeRPS     := flag.Int("probe-rps", 20, "Probe requests per second during scenario")
	zones := zonesFlag{
		"zone1": {"app-zone1-1", "app-zone1-2"},
		"zone2": {"app-zone2-1", "app-zone2-2"},
	}
	flag.Var(zones, "zone", "Zone containers: -zone name=c1,c2 (repeatable)")
	flag.Parse()

	if flag.NArg() == 0 {
		printUsage()
		os.Exit(1)
	}
	scenarioName := flag.Arg(0)
	sc, ok := scenarios[scenarioName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n\nAvailable scenarios:\n", scenarioName)
		for k, s := range scenarios {
			fmt.Fprintf(os.Stderr, "  %-20s %s\n", k, s.description)
		}
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("=== Failure Runner: %s ===\n%s\n\n", sc.name, sc.description)

	// --- Phase 1: Baseline metrics ---
	fmt.Println("--- Baseline (before failure) ---")
	baseline, baseAvail := collectGatewayStats(*prometheusURL)
	printGatewayStats("baseline", baseline, baseAvail)

	baseRec := &stats.Recorder{}
	probeFor(ctx, *probeURL, *probeRPS, 10*time.Second, baseRec)
	baseSnap := baseRec.Snapshot()
	fmt.Printf("baseline probe: %s\n\n", baseSnap.Report(10*time.Second))

	// --- Phase 2: Inject failure ---
	containers := targetContainers(sc, zones)
	fmt.Printf("--- Injecting failure: %v ---\n", containers)
	if err := injectFailure(*runtime, containers, sc.pause); err != nil {
		fmt.Fprintf(os.Stderr, "failed to inject: %v\n", err)
		os.Exit(1)
	}
	failureStart := time.Now()
	fmt.Println("Failure injected. Probing gateway...")

	// --- Phase 3: Measure during failure ---
	failureRec := &stats.Recorder{}
	probeDur := *restoreAfter
	if probeDur == 0 {
		probeDur = 30 * time.Second
	}
	probeFor(ctx, *probeURL, *probeRPS, probeDur, failureRec)
	failureSnap := failureRec.Snapshot()
	fmt.Printf("\nfailure probe (%s): %s\n", probeDur, failureSnap.Report(probeDur))

	// --- Phase 4: Restore ---
	fmt.Printf("\n--- Restoring containers (failure lasted %s) ---\n", time.Since(failureStart).Round(time.Second))
	if err := restoreContainers(*runtime, containers, sc.pause); err != nil {
		fmt.Fprintf(os.Stderr, "warning: restore failed: %v\n", err)
	}

	// --- Phase 5: Post-restore metrics ---
	time.Sleep(3 * time.Second) // give Envoy time to see healthy upstreams
	fmt.Println("\n--- Post-restore (after failover + recovery) ---")
	postStats, postAvail := collectGatewayStats(*prometheusURL)
	printGatewayStats("post-restore", postStats, postAvail)

	// --- Summary ---
	fmt.Printf("\n=== Summary: %s ===\n", sc.name)
	fmt.Printf("baseline   : %s\n", baseSnap.Report(10*time.Second))
	fmt.Printf("during fail: %s\n", failureSnap.Report(probeDur))
	if postAvail {
		fmt.Printf("post-restore (Prometheus): error_rate=%.2f%%  zone1_rps=%.1f  zone2_rps=%.1f  ejections=%.0f\n",
			postStats.errorRatePct, postStats.zone1RPS, postStats.zone2RPS, postStats.ejections)
	}
	fmt.Printf("\nExpected: error_rate < 5%% (failover kicks in within Envoy retry window)\n")
	if failureSnap.ErrorRate() < 5.0 {
		fmt.Println("PASS: error rate within SLA during failure")
	} else {
		fmt.Printf("FAIL: error rate %.2f%% exceeds 5%% threshold\n", failureSnap.ErrorRate())
	}
}

func targetContainers(sc scenario, zones zonesFlag) []string {
	all := zones[sc.zone]
	if sc.partial && len(all) > 1 {
		return all[:len(all)/2]
	}
	return all
}

func injectFailure(runtime string, containers []string, pause bool) error {
	verb := "stop"
	if pause {
		verb = "pause"
	}
	args := append([]string{verb}, containers...)
	cmd := exec.Command(runtime, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func restoreContainers(runtime string, containers []string, wasPaused bool) error {
	verb := "start"
	if wasPaused {
		verb = "unpause"
	}
	args := append([]string{verb}, containers...)
	cmd := exec.Command(runtime, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// probeFor sends requests at rps for dur and records results into rec.
func probeFor(ctx context.Context, rawURL string, rps int, dur time.Duration, rec *stats.Recorder) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "http", Host: "localhost:10000", Path: "/ping"}
	}
	path := u.Path
	baseURL := u.Scheme + "://" + u.Host
	c := client.New(client.Options{BaseURL: baseURL, Timeout: 5 * time.Second})

	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()
	deadline := time.After(dur)
	userIdx := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			userID := fmt.Sprintf("probe-user-%d", userIdx%10)
			cabinetID := fmt.Sprintf("probe-cabinet-%d", userIdx%3)
			userIdx++
			go func(u, cab string) {
				reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
				defer cancel()
				r := c.Do(reqCtx, path, u, cab)
				rec.Add(r.Latency, r.Err != nil || r.StatusCode >= 500)
			}(userID, cabinetID)
		}
	}
}

// gatewayStats holds a point-in-time snapshot of key gateway metrics from Prometheus.
type gatewayStats struct {
	errorRatePct float64 // global 5xx error rate, percent
	zone1RPS     float64 // inbound RPS on zone1-envoy
	zone2RPS     float64 // inbound RPS on zone2-envoy
	ejections    float64 // active outlier-detection ejections in geo_cluster
}

// collectGatewayStats queries Prometheus for current gateway metrics.
// Returns (stats, false) if Prometheus is unreachable.
func collectGatewayStats(prometheusURL string) (gatewayStats, bool) {
	// Availability check before running queries.
	resp, err := http.Get(prometheusURL + "/-/healthy")
	if err != nil {
		return gatewayStats{}, false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return gatewayStats{}, false
	}

	errRate, _ := queryPrometheus(prometheusURL,
		`sum(rate(envoy_http_downstream_rq_xx{job="envoy-global",envoy_http_conn_manager_prefix="ingress_http",envoy_response_code_class="5"}[1m]))`+
			` / sum(rate(envoy_http_downstream_rq_total{job="envoy-global",envoy_http_conn_manager_prefix="ingress_http"}[1m])) * 100`)
	z1rps, _ := queryPrometheus(prometheusURL,
		`sum(rate(envoy_http_downstream_rq_total{job="envoy-zone1",envoy_http_conn_manager_prefix="zone1_ingress"}[1m]))`)
	z2rps, _ := queryPrometheus(prometheusURL,
		`sum(rate(envoy_http_downstream_rq_total{job="envoy-zone2",envoy_http_conn_manager_prefix="zone2_ingress"}[1m]))`)
	ejections, _ := queryPrometheus(prometheusURL,
		`sum(envoy_cluster_outlier_detection_ejections_active{job="envoy-global",envoy_cluster_name="geo_cluster"})`)

	return gatewayStats{
		errorRatePct: errRate,
		zone1RPS:     z1rps,
		zone2RPS:     z2rps,
		ejections:    ejections,
	}, true
}

// queryPrometheus runs an instant PromQL query and returns the scalar result.
// Returns (0, false) if the query returns no data or the value is NaN/Inf.
func queryPrometheus(baseURL, query string) (float64, bool) {
	resp, err := http.Get(baseURL + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	var r struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Status != "success" || len(r.Data.Result) == 0 {
		return 0, false
	}

	var s string
	if err := json.Unmarshal(r.Data.Result[0].Value[1], &s); err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func printGatewayStats(label string, s gatewayStats, available bool) {
	if !available {
		fmt.Printf("[%s] Prometheus unavailable — gateway stats not collected\n\n", label)
		return
	}
	fmt.Printf("[%s] error_rate=%.2f%%  zone1_rps=%.1f  zone2_rps=%.1f  ejections=%.0f\n\n",
		label, s.errorRatePct, s.zone1RPS, s.zone2RPS, s.ejections)
}

func printUsage() {
	fmt.Println("Usage: failure-runner [flags] <scenario>")
	fmt.Println("\nScenarios:")
	for k, s := range scenarios {
		fmt.Printf("  %-20s %s\n", k, s.description)
	}
	fmt.Println("\nFlags:")
	flag.PrintDefaults()
}
