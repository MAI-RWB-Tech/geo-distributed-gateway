// This file implements the Redis Pub/Sub channel for ML-derived routing hints
// (per-zone weights + rate limits). It is intentionally separate from
// watcher.go's ServiceConfig pipeline:
//
//   - ServiceConfig (watcher.go) — operator-edited operational params
//     (timeouts, retries, zone) reloaded from a JSON file.
//   - RoutingHints (this file)   — ML-derived adaptive parameters propagated
//     across data centers via Redis Pub/Sub channel "routing:<service>".
//
// The two contracts deliberately do not share types or channels. See plan T7.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RoutingHints carries ML-derived adaptive routing parameters for one service.
// Distinct from ServiceConfig: ServiceConfig is operator-edited operational
// params (timeouts, retries); RoutingHints is ML-derived per-zone weights
// and rate limits, propagated via Redis Pub/Sub.
//
// Field names and JSON tags are the wire contract — consumed by Control Plane
// (publisher) and any future subscriber (e.g., a self-throttling app worker).
type RoutingHints struct {
	Service    string             `json:"service"`
	Version    int                `json:"version"`
	UpdatedAt  time.Time          `json:"updated_at"`
	Weights    map[string]float64 `json:"weights"`     // zone1 / zone2 → weight
	RateLimits map[string]int     `json:"rate_limits"` // example: {"rps": 750}
}

// RoutingHintsSubscriber subscribes to the "routing:<service>" Redis Pub/Sub
// channel and emits parsed RoutingHints on its Updates() channel.
//
// The emit channel is buffered (size 1) with drop-stale semantics — same
// pattern as Watcher.Subscribe: a slow consumer never blocks the receive
// goroutine and never sees outdated values mixed with fresh ones.
type RoutingHintsSubscriber struct {
	service string
	client  *redis.Client
	pubsub  *redis.PubSub

	updates chan RoutingHints

	cancel context.CancelFunc
	done   chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// NewRoutingHintsSubscriber connects to Redis using a redis-go URL or "host:port"
// short form, subscribes to "routing:<service>", and starts a background
// goroutine that decodes messages into RoutingHints.
//
// The URL may be either:
//   - A full redis-go URL (e.g. "redis://localhost:6379/0")
//   - A bare "host:port" form (e.g. "redis:6379") — this is what
//     docker-compose ships in REDIS_URL.
//
// Caller MUST invoke Close to release the underlying connection and goroutine.
func NewRoutingHintsSubscriber(redisURL, service string) (*RoutingHintsSubscriber, error) {
	if service == "" {
		return nil, fmt.Errorf("service must not be empty")
	}
	opts, err := parseRedisURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}
	client := redis.NewClient(opts)

	// Confirm connectivity up-front so callers see init-time failures rather
	// than silent no-deliveries. 3s is generous for a same-host docker network.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	channel := "routing:" + service

	ctx, cancel := context.WithCancel(context.Background())
	pubsub := client.Subscribe(ctx, channel)

	// Subscribe is lazy; force the round-trip so the subscription is live
	// before we return — otherwise an immediate publish would be missed.
	if _, err := pubsub.Receive(ctx); err != nil {
		cancel()
		_ = pubsub.Close()
		_ = client.Close()
		return nil, fmt.Errorf("redis subscribe %q: %w", channel, err)
	}

	s := &RoutingHintsSubscriber{
		service: service,
		client:  client,
		pubsub:  pubsub,
		updates: make(chan RoutingHints, 1),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go s.loop(ctx)
	return s, nil
}

// Updates returns a channel that receives every fresh RoutingHints decoded
// from the subscribed Redis channel. Buffered (size 1) with drop-stale —
// slow consumers see only the latest value.
func (s *RoutingHintsSubscriber) Updates() <-chan RoutingHints {
	return s.updates
}

// Close stops the subscriber, closes the Redis connection and the Updates()
// channel. It is safe to call more than once; subsequent calls return the
// first close error.
func (s *RoutingHintsSubscriber) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		// Closing pubsub unblocks any in-flight ReceiveMessage.
		if err := s.pubsub.Close(); err != nil {
			s.closeErr = err
		}
		<-s.done
		if err := s.client.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// loop reads Redis pub/sub messages and emits decoded RoutingHints.
// Exit conditions: context cancel (Close), or ReceiveMessage returns an
// unrecoverable error (e.g. connection closed). Decode errors are logged
// and the loop continues — one bad message must not poison the channel.
func (s *RoutingHintsSubscriber) loop(ctx context.Context) {
	defer close(s.done)
	defer close(s.updates)

	ch := s.pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var hint RoutingHints
			if err := json.Unmarshal([]byte(msg.Payload), &hint); err != nil {
				slog.Warn("routing hints: decode failed",
					slog.String("channel", msg.Channel),
					slog.Any("err", err),
				)
				continue
			}
			// Non-blocking send with drop-stale (same shape as
			// watcher.go:99-114). The buffered channel keeps at most one
			// pending value so slow consumers always observe the latest.
			select {
			case s.updates <- hint:
			default:
				select {
				case <-s.updates:
				default:
				}
				// Guard the re-send on ctx so a stalled consumer can't pin this
				// goroutine on a blocking send during Close(). Single writer +
				// buffer 1 makes the bare send safe today; the guard keeps it
				// safe if the buffer size is ever changed.
				select {
				case s.updates <- hint:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// parseRedisURL accepts both a full redis-go URL and a bare "host:port"
// short form (e.g. "redis:6379") that docker-compose conventionally puts
// in REDIS_URL.
func parseRedisURL(s string) (*redis.Options, error) {
	if s == "" {
		return nil, fmt.Errorf("empty redis URL")
	}
	// Heuristic: if the value already looks like a full URL, defer to redis.ParseURL.
	if strings.HasPrefix(s, "redis://") || strings.HasPrefix(s, "rediss://") {
		return redis.ParseURL(s)
	}
	// Bare host:port — wrap it with the default scheme so ParseURL handles
	// the rest (which avoids us re-implementing TLS/db/credentials handling).
	return redis.ParseURL("redis://" + s)
}
