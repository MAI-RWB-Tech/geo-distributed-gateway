// traffic-gen is a CLI tool for generating HTTP load against the geo-distributed gateway.
//
// Usage:
//
//	traffic-gen [flags]
//
// Flags:
//
//	-url      Gateway base URL (default: http://localhost:10000)
//	-path     Request path (default: /ping)
//	-rps      Target requests per second (default: 100)
//	-dur      Test duration, e.g. 30s, 5m (default: 30s)
//	-cabinets Number of distinct cabinet IDs to cycle through (default: 10)
//	-users    Number of users per cabinet (default: 5)
//	-seed     Random seed for reproducibility (default: 42)
//	-report   Interval for live stats reporting (default: 5s)
//	-zone     Optional x-geo header value to pin to a zone ("zone1" | "zone2")
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/geo-distributed-gateway/sdk/client"
	"github.com/geo-distributed-gateway/sdk/stats"
)

func main() {
	url := flag.String("url", "http://localhost:10000", "Gateway base URL")
	path := flag.String("path", "/ping", "Request path")
	rps := flag.Int("rps", 100, "Target requests per second")
	dur := flag.Duration("dur", 30*time.Second, "Test duration")
	cabinets := flag.Int("cabinets", 10, "Number of distinct cabinet IDs")
	usersPerCabinet := flag.Int("users", 5, "Users per cabinet")
	seed := flag.Int64("seed", 42, "Random seed (use same seed to replay identical load)")
	reportInterval := flag.Duration("report", 5*time.Second, "Live stats reporting interval")
	zone := flag.String("zone", "", "Pin to zone via x-geo header (zone1 | zone2 | empty=weighted)")
	flag.Parse()

	if *rps <= 0 {
		fmt.Fprintln(os.Stderr, "rps must be > 0")
		os.Exit(1)
	}

	rng := rand.New(rand.NewSource(*seed))

	// Pre-generate a pool of user/cabinet pairs so the load pattern is
	// deterministic for a given seed (Replay & Seed requirement).
	type identity struct{ userID, cabinetID string }
	poolSize := *cabinets * *usersPerCabinet
	if poolSize < 1 {
		poolSize = 1
	}
	pool := make([]identity, poolSize)
	for i := range pool {
		cabIdx := i / *usersPerCabinet
		pool[i] = identity{
			userID:    fmt.Sprintf("user-%d", i),
			cabinetID: fmt.Sprintf("cabinet-%d", cabIdx),
		}
	}
	// Shuffle once with the seeded RNG so pool order is seed-determined.
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	// Build optional extra headers (zone pinning).
	var extraHeaders map[string]string
	if *zone != "" {
		extraHeaders = map[string]string{"x-geo": *zone}
	}

	c := client.New(client.Options{BaseURL: *url, Timeout: 5 * time.Second})
	rec := &stats.Recorder{}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ticker := time.NewTicker(time.Second / time.Duration(*rps))
	defer ticker.Stop()

	deadline := time.After(*dur)
	reportTicker := time.NewTicker(*reportInterval)
	defer reportTicker.Stop()

	start := time.Now()
	var wg sync.WaitGroup
	idx := 0

	fmt.Printf("traffic-gen  url=%s path=%s rps=%d dur=%s seed=%d cabinets=%d zone=%q\n\n",
		*url, *path, *rps, *dur, *seed, *cabinets, *zone)

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-deadline:
			break loop
		case <-reportTicker.C:
			snap := rec.Snapshot()
			fmt.Printf("[%5.0fs] %s\n", time.Since(start).Seconds(), snap.Report(time.Since(start)))
		case <-ticker.C:
			id := pool[idx%len(pool)]
			idx++
			wg.Add(1)
			go func(userID, cabinetID string) {
				defer wg.Done()
				reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
				defer reqCancel()
				result := c.DoWithHeaders(reqCtx, *path, userID, cabinetID, extraHeaders)
				isErr := result.Err != nil || result.StatusCode >= 500
				rec.Add(result.Latency, isErr)
			}(id.userID, id.cabinetID)
		}
	}

	wg.Wait()
	elapsed := time.Since(start)
	snap := rec.Snapshot()
	fmt.Printf("\n=== Final Report (elapsed: %s) ===\n%s\n", elapsed.Round(time.Millisecond), snap.Report(elapsed))
}
