// events-collector subscribes to stdout of every Docker container labelled
// `gateway.component=app`, parses JSON telemetry events emitted by the
// sdk/telemetry collector, and exposes them as Prometheus metrics on :9100.
//
// Cardinality is intentionally bounded: only {service, zone, kind|status_class}
// appear as labels. Per-user/cabinet detail belongs in Jaeger spans, not Prometheus.
//
// The service does NOT subscribe to docker `events` for late-joining
// containers — v1 only tracks containers that exist at startup. Container
// crashes are handled implicitly: ContainerLogs(Follow=true) returns EOF
// when the container exits, the stream goroutine logs and returns.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/geo-distributed-gateway/sdk/telemetry"
)

const (
	// componentLabel is the docker label set on every app container by
	// docker-compose.yml.
	componentLabel = "gateway.component=app"
)

// metrics groups every Prometheus collector exported by this service.
// Keeping them in a single struct makes it easy to register them all
// at once and to inject a custom registry from tests later.
type metrics struct {
	events   *prometheus.CounterVec
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	latency  *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "events_total",
			Help: "Total telemetry events received from app containers, by service/zone/kind.",
		}, []string{"service", "zone", "kind"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "requests_total",
			Help: "Total request events, by service/zone/status_class (2xx,3xx,4xx,5xx).",
		}, []string{"service", "zone", "status_class"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "errors_total",
			Help: "Total error events emitted by app containers, by service/zone.",
		}, []string{"service", "zone"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "request_latency_ms",
			Help:    "Per-request latency in milliseconds reported by app containers.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		}, []string{"service", "zone"}),
	}
	reg.MustRegister(m.events, m.requests, m.errors, m.latency)
	return m
}

// statusClass collapses a raw HTTP status code into one of "2xx"/"3xx"/
// "4xx"/"5xx"/"other" to keep label cardinality bounded. Codes outside
// 100..599 are reported as "other" so that malformed events never blow
// up the time-series count.
func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return "other"
	}
}

func (m *metrics) record(e telemetry.Event) {
	// Defensive: events without service/zone are dropped — Prometheus
	// labels of "" would still create a series but break joins.
	if e.ServiceName == "" || e.Zone == "" {
		return
	}
	kind := string(e.Kind)
	m.events.WithLabelValues(e.ServiceName, e.Zone, kind).Inc()
	switch e.Kind {
	case telemetry.EventRequest:
		m.requests.WithLabelValues(e.ServiceName, e.Zone, statusClass(e.StatusCode)).Inc()
		if e.LatencyMs > 0 {
			m.latency.WithLabelValues(e.ServiceName, e.Zone).Observe(e.LatencyMs)
		}
	case telemetry.EventError:
		m.errors.WithLabelValues(e.ServiceName, e.Zone).Inc()
	}
}

// streamContainer pumps a single container's stdout into m.
// It returns when ctx is cancelled, when the docker stream closes
// (container exit), or on a fatal read error.
func streamContainer(ctx context.Context, cli *client.Client, c container.Summary, since string, m *metrics, log *slog.Logger) {
	name := containerName(c)
	cl := log.With(slog.String("container", name), slog.String("id", c.ID[:12]))

	rc, err := cli.ContainerLogs(ctx, c.ID, container.LogsOptions{
		ShowStdout: true,
		// stderr is application logs (slog); we do NOT scrape it for events.
		ShowStderr: false,
		Follow:     true,
		Since:      since,
	})
	if err != nil {
		cl.Error("ContainerLogs failed", slog.Any("err", err))
		return
	}
	defer rc.Close()

	// Docker multiplexes stdout/stderr into a framed stream when the container
	// has no TTY. stdcopy.StdCopy demultiplexes; we write stdout into pr and
	// discard stderr (we asked Docker for stdout only, but the framing still
	// exists and StdCopy will skip empty stderr frames cleanly).
	pr, pw := io.Pipe()
	go func() {
		// stdcopy.StdCopy returns when the source EOFs or the pipe errors.
		_, copyErr := stdcopy.StdCopy(pw, io.Discard, rc)
		// Propagate the result to the reader side. If copyErr is nil
		// (clean EOF) the reader will see io.EOF.
		_ = pw.CloseWithError(copyErr)
	}()

	scanner := bufio.NewScanner(pr)
	// Telemetry events are small, but bump the max token to be safe against
	// long correlation_id / error_msg fields.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	cl.Info("subscribed to container logs")

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			// Not JSON (e.g. raw stdout from a non-telemetry writer) — skip.
			continue
		}
		var ev telemetry.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Malformed JSON line — ignore silently.
			continue
		}
		m.record(ev)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		cl.Warn("log stream ended with error", slog.Any("err", err))
	} else {
		cl.Info("log stream ended")
	}
}

// containerName returns the friendly name of a container (without the
// leading slash docker prepends), falling back to the short ID.
func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		n := c.Names[0]
		if len(n) > 0 && n[0] == '/' {
			n = n[1:]
		}
		return n
	}
	return c.ID[:12]
}

func main() {
	listenAddr := flag.String("listen", ":9100", "HTTP listen address for /metrics and /healthz")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	log := slog.Default()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Error("docker client init failed", slog.Any("err", err))
		os.Exit(1)
	}
	defer cli.Close()

	reg := prometheus.NewRegistry()
	// Register Go runtime + process collectors so basic health is visible
	// alongside our domain metrics (memory, goroutines, fd count).
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	m := newMetrics(reg)

	// List containers labelled as app instances. We subscribe to whatever
	// exists at startup; v1 does not handle late joiners.
	args := filters.NewArgs()
	args.Add("label", componentLabel)
	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     false, // running only — exited containers can't produce new events
		Filters: args,
	})
	if err != nil {
		log.Error("ContainerList failed", slog.Any("err", err))
		os.Exit(1)
	}
	log.Info("discovered app containers",
		slog.Int("count", len(containers)),
		slog.String("label", componentLabel),
	)

	// `Since` is the wall-clock moment events-collector subscribed. Docker
	// will still deliver logs older than this if Follow=true and the
	// container was idle, but typical app containers start emitting `start`
	// events at boot — those reach us once the apps come up after we
	// subscribe (events-collector has no depends_on, so it boots first).
	since := strconv.FormatInt(time.Now().Unix(), 10)

	var wg sync.WaitGroup
	for _, c := range containers {
		wg.Add(1)
		go func(c container.Summary) {
			defer wg.Done()
			streamContainer(ctx, cli, c, since, m, log)
		}(c)
	}

	// HTTP surface: /metrics for Prometheus scrape, /healthz for liveness.
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	srvErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", *listenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-srvErr:
		if err != nil {
			log.Error("http server failed", slog.Any("err", err))
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown error", slog.Any("err", err))
	}

	// Cancelling the root context closes every ContainerLogs stream.
	stop()
	wg.Wait()
	log.Info("events-collector stopped")
}
