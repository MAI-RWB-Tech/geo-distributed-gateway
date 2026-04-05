package config_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/geo-distributed-gateway/sdk/config"
)

func writeConfig(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatcher_Initial(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cfg*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	writeConfig(t, f.Name(), map[string]any{
		"request_timeout": "3s",
		"max_retries":     5,
		"retry_backoff":   "100ms",
		"zone":            "zone1",
	})

	w, err := config.NewWatcher(f.Name(), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	cfg := w.Get()
	if cfg.RequestTimeout != 3*time.Second {
		t.Errorf("request_timeout: want 3s, got %s", cfg.RequestTimeout)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("max_retries: want 5, got %d", cfg.MaxRetries)
	}
	if cfg.Zone != "zone1" {
		t.Errorf("zone: want zone1, got %s", cfg.Zone)
	}
}

func TestWatcher_HotReload(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cfg*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	writeConfig(t, f.Name(), map[string]any{"request_timeout": "2s", "max_retries": 2, "retry_backoff": "50ms"})

	w, err := config.NewWatcher(f.Name(), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	sub := w.Subscribe()

	// Update the file.
	writeConfig(t, f.Name(), map[string]any{"request_timeout": "7s", "max_retries": 10, "retry_backoff": "500ms"})

	select {
	case updated := <-sub:
		if updated.RequestTimeout != 7*time.Second {
			t.Errorf("hot reload: want 7s, got %s", updated.RequestTimeout)
		}
		if updated.MaxRetries != 10 {
			t.Errorf("hot reload max_retries: want 10, got %d", updated.MaxRetries)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for config update notification")
	}
}
