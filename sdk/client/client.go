// Package client provides an HTTP client SDK for the geo-distributed gateway.
// It handles X-User-ID / X-Cabinet-ID header injection, correlation IDs, and per-request timing.
package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// correlationCounter is used to generate monotonically increasing correlation IDs.
var correlationCounter atomic.Uint64

// Options configures a Client.
type Options struct {
	// BaseURL is the Envoy gateway base URL, e.g. "http://localhost:10000".
	BaseURL string
	// Timeout per request. Defaults to 5 s.
	Timeout time.Duration
	// HTTPClient allows injecting a custom *http.Client (useful for tests).
	HTTPClient *http.Client
}

func (o *Options) defaults() {
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.Timeout}
	} else {
		// Apply Timeout to the provided client; transport and other settings are preserved.
		o.HTTPClient.Timeout = o.Timeout
	}
}

// Client sends requests to the gateway with mandatory routing headers.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a Client from Options.
func New(opts Options) *Client {
	opts.defaults()
	return &Client{baseURL: opts.BaseURL, http: opts.HTTPClient}
}

// Result holds the outcome of a single request.
type Result struct {
	StatusCode    int
	Latency       time.Duration
	CorrelationID string
	Err           error
}

// Do performs a GET request to path and returns a Result.
// userID and cabinetID are propagated in X-User-ID / X-Cabinet-ID headers.
// An X-Correlation-ID header is auto-generated and echoed back in Result.
func (c *Client) Do(ctx context.Context, path, userID, cabinetID string) Result {
	return c.DoWithHeaders(ctx, path, userID, cabinetID, nil)
}

// DoWithHeaders is like Do but also sets additional HTTP headers.
// Keys in extra override any default header with the same name.
func (c *Client) DoWithHeaders(ctx context.Context, path, userID, cabinetID string, extra map[string]string) Result {
	correlationID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), correlationCounter.Add(1))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return Result{Err: fmt.Errorf("build request: %w", err), CorrelationID: correlationID}
	}

	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Cabinet-ID", cabinetID)
	req.Header.Set("X-Correlation-ID", correlationID)
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	latency := time.Since(start)

	if err != nil {
		return Result{Latency: latency, Err: fmt.Errorf("do request: %w", err), CorrelationID: correlationID}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain body to reuse connection

	return Result{
		StatusCode:    resp.StatusCode,
		Latency:       latency,
		CorrelationID: correlationID,
	}
}
