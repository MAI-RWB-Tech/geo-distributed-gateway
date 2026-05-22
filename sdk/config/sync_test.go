package config_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/geo-distributed-gateway/sdk/config"
)

// TestRoutingHintsSubscriber_PublishDeliveredAndParsed exercises the happy
// path: a subscriber subscribed to routing:<service> receives messages
// published on that channel, decoded into the RoutingHints struct.
func TestRoutingHintsSubscriber_PublishDeliveredAndParsed(t *testing.T) {
	srv := miniredis.RunT(t)

	sub, err := config.NewRoutingHintsSubscriber(srv.Addr(), "service-a")
	if err != nil {
		t.Fatalf("NewRoutingHintsSubscriber: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// Publish a single RoutingHints message via a separate client connection.
	publisher := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = publisher.Close() })

	want := config.RoutingHints{
		Service:    "service-a",
		Version:    7,
		UpdatedAt:  time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC),
		Weights:    map[string]float64{"zone1": 0.7, "zone2": 0.3},
		RateLimits: map[string]int{"rps": 750},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal hint: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Give the subscriber a brief moment to settle so the publish lands
	// after Subscribe registration on the miniredis side.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _ := publisher.Publish(ctx, "routing:service-a", body).Result(); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no subscriber observed the publish before deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case got, ok := <-sub.Updates():
		if !ok {
			t.Fatal("Updates channel closed before delivering a message")
		}
		if got.Service != want.Service {
			t.Errorf("service: want %q, got %q", want.Service, got.Service)
		}
		if got.Version != want.Version {
			t.Errorf("version: want %d, got %d", want.Version, got.Version)
		}
		if !got.UpdatedAt.Equal(want.UpdatedAt) {
			t.Errorf("updated_at: want %s, got %s", want.UpdatedAt, got.UpdatedAt)
		}
		if got.Weights["zone1"] != 0.7 || got.Weights["zone2"] != 0.3 {
			t.Errorf("weights: want %v, got %v", want.Weights, got.Weights)
		}
		if got.RateLimits["rps"] != 750 {
			t.Errorf("rate_limits.rps: want 750, got %d", got.RateLimits["rps"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for routing hint")
	}
}

// TestRoutingHintsSubscriber_BadJSONSkipped verifies a malformed message
// is logged and discarded — the channel must stay alive for subsequent
// well-formed messages.
func TestRoutingHintsSubscriber_BadJSONSkipped(t *testing.T) {
	srv := miniredis.RunT(t)

	sub, err := config.NewRoutingHintsSubscriber(srv.Addr(), "service-b")
	if err != nil {
		t.Fatalf("NewRoutingHintsSubscriber: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	publisher := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = publisher.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Wait for the subscription to be live (Receive() in init already
	// guarantees subscribe ACK; publish still needs ≥1 subscriber to be
	// reflected in the result count from miniredis).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _ := publisher.Publish(ctx, "routing:service-b", "not-json").Result(); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscriber never became visible to publisher")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A well-formed message after the bad one MUST still be delivered.
	good := config.RoutingHints{Service: "service-b", Version: 1, UpdatedAt: time.Now().UTC()}
	body, _ := json.Marshal(good)
	if _, err := publisher.Publish(ctx, "routing:service-b", body).Result(); err != nil {
		t.Fatalf("publish good: %v", err)
	}

	select {
	case got := <-sub.Updates():
		if got.Service != "service-b" || got.Version != 1 {
			t.Errorf("unexpected hint after recovery: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: bad message poisoned the channel")
	}
}

// TestRoutingHintsSubscriber_CloseTerminatesUpdates ensures Close() shuts
// down the goroutine cleanly and the Updates channel observes the close.
func TestRoutingHintsSubscriber_CloseTerminatesUpdates(t *testing.T) {
	srv := miniredis.RunT(t)

	sub, err := config.NewRoutingHintsSubscriber(srv.Addr(), "service-c")
	if err != nil {
		t.Fatalf("NewRoutingHintsSubscriber: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent Close — second call should not panic.
	if err := sub.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	select {
	case _, ok := <-sub.Updates():
		if ok {
			t.Errorf("Updates returned a value after Close")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Updates channel not closed after Close()")
	}
}
