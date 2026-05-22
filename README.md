# Geo-Distributed Gateway

Two-DC HTTP gateway in Docker Compose: 10 stub services, Envoy data plane, Consul discovery, ML-driven adaptive routing.

## Requirements

- Docker 25+ (or compatible)
- GNU Make
- Free TCP ports: 3000, 4318, 6379, 8500, 9090, 9093, 9100, 9200, 9300, 10000, 16686, 19000-19002
- Free Docker subnets: 172.30.0.0/24, 172.30.1.0/24, 172.30.2.0/24

## Quick start

```bash
make up         # build + start full stack (~21 containers)
make status     # docker compose ps
make traffic-gen   # 100 RPS x 30s smoke test
make down       # stop + remove
```

## Endpoints

| Service | Port | What |
|---|---|---|
| Global Envoy (public) | 10000 | `GET /service-{a..e}/ping`, `GET /ping` |
| Consul UI | 8500 | Service catalog |
| Jaeger UI | 16686 | Distributed traces |
| Prometheus | 9090 | Metrics + alerts |
| Grafana | 3000 | Dashboards (anonymous admin) |
| Alertmanager | 9093 | Alert routing |
| Events Collector | 9100 | `/metrics`, `/healthz` |
| ML Analyzer | 9200 | `/recommendations`, `/healthz`, `/metrics` |
| Control Plane | 9300 | `/config`, `/config/weights/{service}`, `/healthz`, `/metrics` |
| Envoy admin | 19000 (global), 19001 (zone1), 19002 (zone2) | `/stats`, `/clusters`, `/config_dump` |

Full HTTP API spec: [`docs/openapi.yaml`](docs/openapi.yaml).

## Features

- **10 stub services** across 2 zones (`service-{a..e} x zone{1,2}`) with self-registration in Consul on startup.
- **Tag-based Consul DNS discovery** (`<zone>.<service>.service.consul`) consumed by zone-envoy via static resolver.
- **Global Envoy** path-routes `/service-{a..e}/*` to a shared `geo_cluster` with `outlier_detection` for cross-zone failover.
- **Lua score filter** in global-envoy: per-request weighted zone pinning via `x-geo` header, weights fetched from Control Plane with 30s per-worker cache and 50/50 fallback.
- **Events Collector**: Docker SDK subscribes to stdout of containers labelled `gateway.component=app`, parses telemetry JSON Lines, exposes Prometheus metrics with bounded cardinality (no `user_id` / `cabinet_id`).
- **ML Analyzer**: PromQL-based heuristic (`score = 1 / (1 + p99_ms/100) * (1 - error_rate)`), publishes JSON recommendations on `:9200/recommendations`.
- **Control Plane**: polls ML every 30s, exposes versioned snapshot on `:9300/config`, publishes to Redis `routing:<service>` Pub/Sub on content change.
- **Routing hints subscriber** in SDK: app services consume `routing:<service>` and log received hints (v1 — observability only).
- **OTel tracing** end-to-end: `traffic-gen` → `global-envoy` → `service-X`, exported to Jaeger via OTLP HTTP. Server spans carry `user_id`, `cabinet_id`, `zone` attributes.
- **Chaos tools**: `make failure-zone1`, `make failure-partial` exercise zone outage; `make traffic-gen` for synthetic load.

## Testing

```bash
# Per-module Go tests (run inside a Go 1.26 container if Go is not local):
cd sdk && go test ./...
cd app && go test ./...
cd cmd/ml-analyzer && go test ./...

# End-to-end smoke:
make up && make traffic-gen && make failure-zone1
```

## Repository layout

```
app/                  Stub HTTP service
sdk/                  Shared library (client, config, stats, telemetry)
cmd/events-collector/ Docker logs -> Prometheus
cmd/ml-analyzer/      Prometheus -> recommendations JSON
cmd/control-plane/    ML -> snapshot + Redis publish
cmd/traffic-gen/      Load generator
cmd/failure-runner/   Chaos / failover harness
envoy/                3 Envoy configs (global + zone1 + zone2) + Lua filter
monitoring/           Prometheus + Grafana + Alertmanager configs
docs/                 ADR, Requirements, Runbook, OpenAPI
```

## Architecture

See [`docs/ADR.md`](docs/ADR.md) for design rationale and [`docs/Runbook.md`](docs/Runbook.md) for operational procedures.
