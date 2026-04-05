// Package telemetry provides structured event publishing for gateway services.
// Events are written as JSON lines to a configurable writer (stdout by default).
// The collector (Events Collector role) reads these from stdout/file.
package telemetry

import (
	"encoding/json"
	"io"
	"os"
	"time"
)

// EventKind describes the type of lifecycle event.
type EventKind string

const (
	EventStart   EventKind = "start"
	EventStop    EventKind = "stop"
	EventRequest EventKind = "request"
	EventError   EventKind = "error"
)

// Event is the canonical telemetry event shape (v1).
type Event struct {
	Version       int       `json:"v"`
	Kind          EventKind `json:"kind"`
	ServiceName   string    `json:"service"`
	InstanceID    string    `json:"instance,omitempty"`
	Zone          string    `json:"zone,omitempty"`
	UserID        string    `json:"user_id,omitempty"`
	CabinetID     string    `json:"cabinet_id,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	StatusCode    int       `json:"status_code,omitempty"`
	LatencyMs     float64   `json:"latency_ms,omitempty"`
	ErrorMsg      string    `json:"error,omitempty"`
	Timestamp     time.Time `json:"ts"`
}

// Collector publishes events to an io.Writer as JSON lines.
type Collector struct {
	service  string
	instance string
	zone     string
	out      io.Writer
	enc      *json.Encoder
}

// New creates a Collector for a service.
// Pass nil writer to use os.Stdout.
func New(serviceName, instanceID, zone string, w io.Writer) *Collector {
	if w == nil {
		w = os.Stdout
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &Collector{
		service:  serviceName,
		instance: instanceID,
		zone:     zone,
		out:      w,
		enc:      enc,
	}
}

func (c *Collector) emit(e Event) {
	e.Version = 1
	e.ServiceName = c.service
	e.InstanceID = c.instance
	e.Zone = c.zone
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	_ = c.enc.Encode(e) // best-effort; logging failures are non-fatal
}

// Start records that the service has started.
func (c *Collector) Start() {
	c.emit(Event{Kind: EventStart})
}

// Stop records that the service is stopping.
func (c *Collector) Stop() {
	c.emit(Event{Kind: EventStop})
}

// Request records the outcome of a single handled request.
func (c *Collector) Request(userID, cabinetID, correlationID string, statusCode int, latency time.Duration) {
	c.emit(Event{
		Kind:          EventRequest,
		UserID:        userID,
		CabinetID:     cabinetID,
		CorrelationID: correlationID,
		StatusCode:    statusCode,
		LatencyMs:     float64(latency.Microseconds()) / 1000.0,
	})
}

// Error records an error event (no status code means infrastructure-level failure).
func (c *Collector) Error(correlationID, msg string) {
	c.emit(Event{
		Kind:          EventError,
		CorrelationID: correlationID,
		ErrorMsg:      msg,
	})
}
