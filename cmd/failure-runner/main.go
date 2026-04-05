// failure-runner orchestrates failure injection scenarios against the geo-distributed gateway.
// It uses the container runtime CLI (podman/docker stop/start/pause/unpause)
// and collects Envoy admin metrics before and after each scenario to measure impact.
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
//	-admin1         Zone1 Envoy admin URL (default: http://localhost:19001)
//	-admin2         Zone2 Envoy admin URL (default: http://localhost:19002)
//	-restore-after  Auto-restore containers after this duration (0 = manual, default: 30s)
//	-probe-url      Gateway URL to probe during scenario (default: http://localhost:10000/ping)
//	-probe-rps      Probe RPS during scenario (default: 20)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/geo-distributed-gateway/sdk/client"
	"github.com/geo-distributed-gateway/sdk/stats"
)

// zone container groups known to docker-compose.
var zoneContainers = map[string][]string{
	"zone1": {"app-zone1-1", "app-zone1-2"},
	"zone2": {"app-zone2-1", "app-zone2-2"},
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
	runtime := flag.String("runtime", "podman", "Container runtime: podman or docker")
	admin1 := flag.String("admin1", "http://localhost:19001", "Zone1 Envoy admin URL")
	admin2 := flag.String("admin2", "http://localhost:19002", "Zone2 Envoy admin URL")
	restoreAfter := flag.Duration("restore-after", 30*time.Second, "Auto-restore after duration (0 = manual)")
	probeURL := flag.String("probe-url", "http://localhost:10000/ping", "Gateway URL to probe during scenario")
	probeRPS := flag.Int("probe-rps", 20, "Probe requests per second during scenario")
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
	baseline := collectEnvoyStats(*admin1, *admin2)
	printStats("baseline", baseline)

	baseRec := &stats.Recorder{}
	probeFor(ctx, *probeURL, *probeRPS, 10*time.Second, baseRec)
	baseSnap := baseRec.Snapshot()
	fmt.Printf("baseline probe: %s\n\n", baseSnap.Report(10*time.Second))

	// --- Phase 2: Inject failure ---
	containers := targetContainers(sc)
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
	postStats := collectEnvoyStats(*admin1, *admin2)
	printStats("post-restore", postStats)

	// --- Summary ---
	fmt.Printf("\n=== Summary: %s ===\n", sc.name)
	fmt.Printf("baseline   : %s\n", baseSnap.Report(10*time.Second))
	fmt.Printf("during fail: %s\n", failureSnap.Report(probeDur))
	fmt.Printf("\nExpected: error_rate < 5%% (failover kicks in within Envoy retry window)\n")
	if failureSnap.ErrorRate() < 5.0 {
		fmt.Println("PASS: error rate within SLA during failure")
	} else {
		fmt.Printf("FAIL: error rate %.2f%% exceeds 5%% threshold\n", failureSnap.ErrorRate())
	}

}

func targetContainers(sc scenario) []string {
	all := zoneContainers[sc.zone]
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
func probeFor(ctx context.Context, url string, rps int, dur time.Duration, rec *stats.Recorder) {
	c := client.New(client.Options{BaseURL: strings.TrimSuffix(url, "/ping"), Timeout: 5 * time.Second})
	path := "/ping"
	if idx := strings.LastIndex(url, "/"); idx >= 0 {
		path = url[idx:]
	}

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

type envoyStats struct {
	zone1RX, zone2RX string // raw /stats output snippet
}

func collectEnvoyStats(admin1, admin2 string) envoyStats {
	return envoyStats{
		zone1RX: fetchAdminStats(admin1),
		zone2RX: fetchAdminStats(admin2),
	}
}

func fetchAdminStats(adminURL string) string {
	resp, err := http.Get(adminURL + "/stats?filter=upstream_rq_total")
	if err != nil {
		return fmt.Sprintf("(unavailable: %v)", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

func printStats(label string, s envoyStats) {
	fmt.Printf("[%s] zone1-envoy stats:\n%s\n", label, indent(s.zone1RX))
	fmt.Printf("[%s] zone2-envoy stats:\n%s\n\n", label, indent(s.zone2RX))
}

func indent(s string) string {
	if s == "" {
		return "  (no data)"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
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
