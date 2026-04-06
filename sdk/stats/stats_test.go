package stats_test

import (
	"testing"
	"time"

	"github.com/geo-distributed-gateway/sdk/stats"
)

func TestRecorder_Percentiles(t *testing.T) {
	r := &stats.Recorder{}
	for i := 1; i <= 100; i++ {
		r.Add(time.Duration(i)*time.Millisecond, false)
	}
	snap := r.Snapshot()

	if snap.Total() != 100 {
		t.Errorf("total: want 100, got %d", snap.Total())
	}
	if snap.Errors() != 0 {
		t.Errorf("errors: want 0, got %d", snap.Errors())
	}
	if snap.ErrorRate() != 0 {
		t.Errorf("error rate: want 0, got %f", snap.ErrorRate())
	}

	// p50 should be ~50ms, p95 ~95ms, p99 ~99ms
	p50 := snap.P50()
	if p50 < 49*time.Millisecond || p50 > 51*time.Millisecond {
		t.Errorf("p50: want ~50ms, got %s", p50)
	}
	p95 := snap.P95()
	if p95 < 94*time.Millisecond || p95 > 96*time.Millisecond {
		t.Errorf("p95: want ~95ms, got %s", p95)
	}
	p99 := snap.P99()
	if p99 < 98*time.Millisecond || p99 > 100*time.Millisecond {
		t.Errorf("p99: want ~99ms, got %s", p99)
	}
}

func TestRecorder_Errors(t *testing.T) {
	r := &stats.Recorder{}
	r.Add(10*time.Millisecond, false)
	r.Add(10*time.Millisecond, false)
	r.Add(10*time.Millisecond, true)

	snap := r.Snapshot()
	if snap.Total() != 3 {
		t.Errorf("total: want 3, got %d", snap.Total())
	}
	if snap.Errors() != 1 {
		t.Errorf("errors: want 1, got %d", snap.Errors())
	}
	wantRate := 100.0 / 3.0
	if snap.ErrorRate() < wantRate-0.1 || snap.ErrorRate() > wantRate+0.1 {
		t.Errorf("error rate: want ~%.2f%%, got %.2f%%", wantRate, snap.ErrorRate())
	}
}

func TestRecorder_Empty(t *testing.T) {
	r := &stats.Recorder{}
	snap := r.Snapshot()
	if snap.P50() != 0 || snap.P95() != 0 || snap.P99() != 0 {
		t.Error("empty recorder should return 0 for all percentiles")
	}
}

func TestRecorder_Reset(t *testing.T) {
	r := &stats.Recorder{}
	r.Add(10*time.Millisecond, false)
	r.Reset()
	snap := r.Snapshot()
	if snap.Total() != 0 {
		t.Errorf("after reset total: want 0, got %d", snap.Total())
	}
}
