// Package stats provides latency histogram and percentile calculation
// used by the traffic generator and failure runner.
package stats

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Recorder accumulates latency samples and counts errors.
// All methods are safe for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	latencies []time.Duration
	errors    int
	total     int
}

// Add records a single result.
func (r *Recorder) Add(latency time.Duration, isError bool) {
	r.mu.Lock()
	r.total++
	if isError {
		r.errors++
	} else {
		r.latencies = append(r.latencies, latency)
	}
	r.mu.Unlock()
}

// Snapshot returns a point-in-time copy of the accumulated data.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	lat := make([]time.Duration, len(r.latencies))
	copy(lat, r.latencies)
	total := r.total
	errs := r.errors
	r.mu.Unlock()

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return Snapshot{latencies: lat, total: total, errors: errs}
}

// Reset clears all accumulated data.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.latencies = r.latencies[:0]
	r.total = 0
	r.errors = 0
	r.mu.Unlock()
}

// Snapshot is an immutable view of recorded stats.
type Snapshot struct {
	latencies []time.Duration
	total     int
	errors    int
}

func (s Snapshot) Total() int   { return s.total }
func (s Snapshot) Errors() int  { return s.errors }
func (s Snapshot) Success() int { return s.total - s.errors }

func (s Snapshot) ErrorRate() float64 {
	if s.total == 0 {
		return 0
	}
	return float64(s.errors) / float64(s.total) * 100
}

// Percentile returns the p-th percentile latency (0–100).
// Returns 0 if there are no successful samples.
func (s Snapshot) Percentile(p float64) time.Duration {
	n := len(s.latencies)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100.0*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return s.latencies[idx]
}

// P50 returns median latency.
func (s Snapshot) P50() time.Duration { return s.Percentile(50) }

// P95 returns p95 latency.
func (s Snapshot) P95() time.Duration { return s.Percentile(95) }

// P99 returns p99 latency.
func (s Snapshot) P99() time.Duration { return s.Percentile(99) }

// Report returns a human-readable summary string.
func (s Snapshot) Report(elapsed time.Duration) string {
	rps := 0.0
	if elapsed > 0 {
		rps = float64(s.total) / elapsed.Seconds()
	}
	return fmt.Sprintf(
		"total=%-6d success=%-6d errors=%-6d error_rate=%.2f%%  rps=%-8.1f  p50=%-10s p95=%-10s p99=%s",
		s.total, s.Success(), s.errors, s.ErrorRate(),
		rps,
		s.P50().Round(time.Microsecond),
		s.P95().Round(time.Microsecond),
		s.P99().Round(time.Microsecond),
	)
}
