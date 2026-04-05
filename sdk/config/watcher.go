// Package config provides a hot-reloading configuration watcher.
// It reads a YAML/JSON file and notifies subscribers on change via channels.
// This lets services pick up updated timeouts/retries without restart.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// ServiceConfig holds tuneable runtime parameters for a service.
type ServiceConfig struct {
	// RequestTimeout is the per-request timeout applied by the client SDK.
	RequestTimeout time.Duration `json:"request_timeout"`
	// MaxRetries is the number of retry attempts on transient failures.
	MaxRetries int `json:"max_retries"`
	// RetryBackoff is the base delay between retries (exponential).
	RetryBackoff time.Duration `json:"retry_backoff"`
	// Zone overrides which zone the service considers primary.
	Zone string `json:"zone,omitempty"`
}

func (c ServiceConfig) equal(other ServiceConfig) bool {
	return c == other
}

// rawConfig is the on-disk JSON/YAML representation with string durations.
type rawConfig struct {
	RequestTimeout string `json:"request_timeout"`
	MaxRetries     int    `json:"max_retries"`
	RetryBackoff   string `json:"retry_backoff"`
	Zone           string `json:"zone,omitempty"`
}

func parse(path string) (ServiceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("read config file: %w", err)
	}
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return ServiceConfig{}, fmt.Errorf("parse config JSON: %w", err)
	}
	cfg := ServiceConfig{MaxRetries: raw.MaxRetries, Zone: raw.Zone}
	if raw.RequestTimeout != "" {
		if cfg.RequestTimeout, err = time.ParseDuration(raw.RequestTimeout); err != nil {
			return ServiceConfig{}, fmt.Errorf("parse request_timeout: %w", err)
		}
	}
	if raw.RetryBackoff != "" {
		if cfg.RetryBackoff, err = time.ParseDuration(raw.RetryBackoff); err != nil {
			return ServiceConfig{}, fmt.Errorf("parse retry_backoff: %w", err)
		}
	}
	return cfg, nil
}

// Watcher polls a config file and publishes updates to subscribers.
type Watcher struct {
	path     string
	interval time.Duration

	mu      sync.RWMutex
	current ServiceConfig
	subs    []chan ServiceConfig

	stop chan struct{}
	done chan struct{}
}

// NewWatcher creates and starts a Watcher for the given file path.
// It polls every interval (e.g. 5s). Call Close() to stop it.
func NewWatcher(path string, interval time.Duration) (*Watcher, error) {
	cfg, err := parse(path)
	if err != nil {
		return nil, fmt.Errorf("initial config load: %w", err)
	}
	w := &Watcher{
		path:     path,
		interval: interval,
		current:  cfg,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go w.loop()
	return w, nil
}

// Get returns the currently active configuration (safe for concurrent use).
func (w *Watcher) Get() ServiceConfig {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

// Subscribe returns a channel that receives a new ServiceConfig whenever
// the config file changes. The channel is buffered (size 1); slow consumers
// receive only the latest value.
func (w *Watcher) Subscribe() <-chan ServiceConfig {
	ch := make(chan ServiceConfig, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

// Close stops the watcher and closes all subscriber channels.
func (w *Watcher) Close() {
	close(w.stop)
	<-w.done
}

func (w *Watcher) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			w.mu.Lock()
			for _, ch := range w.subs {
				close(ch)
			}
			w.mu.Unlock()
			return
		case <-ticker.C:
			cfg, err := parse(w.path)
			if err != nil {
				// Keep current config; errors are transient (file being written).
				continue
			}
			w.mu.Lock()
			if !cfg.equal(w.current) {
				w.current = cfg
				for _, ch := range w.subs {
					// Non-blocking send: drop stale value if consumer is slow.
					select {
					case ch <- cfg:
					default:
						// Drain old value and send fresh one.
						select {
						case <-ch:
						default:
						}
						ch <- cfg
					}
				}
			}
			w.mu.Unlock()
		}
	}
}
