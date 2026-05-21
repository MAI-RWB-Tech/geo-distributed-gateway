package main

import (
	"math"
	"testing"
)

// TestBuildRecommendations_NoData verifies the fallback behaviour: when neither
// Prometheus query returned any data point, every service gets a 0.5/0.5
// even split and the rate-limit floor of 100 rps.
func TestBuildRecommendations_NoData(t *testing.T) {
	rec := buildRecommendations(nil, nil, nil)

	if rec.UpdatedAt == "" {
		t.Fatalf("UpdatedAt must be set")
	}
	for _, svc := range services {
		w, ok := rec.Weights[svc]
		if !ok {
			t.Fatalf("missing weights for %s", svc)
		}
		if math.Abs(w.Zone1-0.5) > 0.001 || math.Abs(w.Zone2-0.5) > 0.001 {
			t.Errorf("%s: expected 0.5/0.5 fallback, got %v/%v", svc, w.Zone1, w.Zone2)
		}
		rl, ok := rec.RateLimits[svc]
		if !ok {
			t.Fatalf("missing rate_limits for %s", svc)
		}
		if rl.RPS <= 0 {
			t.Errorf("%s: rps must be > 0, got %v", svc, rl.RPS)
		}
		if rl.RPS != 100 {
			t.Errorf("%s: expected fallback 100 rps, got %v", svc, rl.RPS)
		}
	}
}

// TestBuildRecommendations_WeightInvariant checks the |z1+z2-1.0| ≤ 0.01
// guarantee with non-trivial latency / error inputs.
func TestBuildRecommendations_WeightInvariant(t *testing.T) {
	p99 := promResult{
		"service-a-zone1-cluster": 50,  // ms
		"service-a-zone2-cluster": 150, // ms — zone2 slower
		"service-b-zone1-cluster": 100,
		"service-b-zone2-cluster": 100, // equal latency
	}
	rps := promResult{
		"service-a-zone1-cluster": 200,
		"service-a-zone2-cluster": 200,
		"service-b-zone1-cluster": 100,
		"service-b-zone2-cluster": 100,
	}
	errs := promResult{
		"service-a-zone1-cluster": 0,
		"service-a-zone2-cluster": 4, // 4/200 = 2% errors in zone2
		"service-b-zone1-cluster": 0,
		"service-b-zone2-cluster": 0,
	}

	rec := buildRecommendations(p99, rps, errs)
	for _, svc := range services {
		w := rec.Weights[svc]
		sum := w.Zone1 + w.Zone2
		if math.Abs(sum-1.0) > 0.01 {
			t.Errorf("%s: weight sum %v not within 0.01 of 1.0", svc, sum)
		}
		if w.Zone1 < 0 || w.Zone2 < 0 {
			t.Errorf("%s: weights must be non-negative, got %v/%v", svc, w.Zone1, w.Zone2)
		}
	}
	// service-a: zone1 faster and clean → should get more weight than zone2.
	if rec.Weights["service-a"].Zone1 <= rec.Weights["service-a"].Zone2 {
		t.Errorf("service-a: zone1 should outweigh zone2 (lower p99, no errors), got %+v",
			rec.Weights["service-a"])
	}
	// service-b: identical metrics → 50/50.
	w := rec.Weights["service-b"]
	if math.Abs(w.Zone1-0.5) > 0.05 || math.Abs(w.Zone2-0.5) > 0.05 {
		t.Errorf("service-b: identical metrics should give ~0.5/0.5, got %+v", w)
	}
}

// TestBuildRecommendations_RateLimitFromMaxRPS checks the rate_limit formula:
// rate_limits.rps = round(max(zone1_rps, zone2_rps) * 1.5).
func TestBuildRecommendations_RateLimitFromMaxRPS(t *testing.T) {
	rps := promResult{
		"service-a-zone1-cluster": 200,
		"service-a-zone2-cluster": 100, // max = 200, * 1.5 = 300
	}
	rec := buildRecommendations(nil, rps, nil)
	got := rec.RateLimits["service-a"].RPS
	if math.Abs(got-300) > 1 {
		t.Errorf("service-a: expected ~300 rps, got %v", got)
	}
	// service-b has no data — fallback to 100.
	if rec.RateLimits["service-b"].RPS != 100 {
		t.Errorf("service-b: expected fallback 100 rps, got %v", rec.RateLimits["service-b"].RPS)
	}
}

// TestReshape_DropsUnknownClusters verifies that cluster names outside the
// `service-[a-e]-zone[12]-cluster` pattern are dropped instead of polluting
// the per-service aggregation.
func TestReshape_DropsUnknownClusters(t *testing.T) {
	in := promResult{
		"service-a-zone1-cluster": 1,
		"service-f-zone1-cluster": 99, // unknown service
		"service-a-zone3-cluster": 99, // unknown zone
		"geo_cluster":             99, // legacy cluster
	}
	out := reshape(in)
	if v := out["service-a"]["zone1"]; v != 1 {
		t.Errorf("expected service-a/zone1 = 1, got %v", v)
	}
	if _, ok := out["service-f"]; ok {
		t.Errorf("service-f must not be present")
	}
	if _, ok := out["service-a"]["zone3"]; ok {
		t.Errorf("zone3 must not be present")
	}
}
