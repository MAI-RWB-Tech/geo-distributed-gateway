# ── Runtime detection ──────────────────────────────────────────────────────────
# Prefer docker if available; fall back to podman.
# Override: make RUNTIME=podman up
RUNTIME ?= $(shell command -v docker 2>/dev/null && echo docker || echo podman)

# Compose command: docker compose (plugin) > docker-compose (legacy) > podman-compose
COMPOSE ?= $(shell \
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then \
    echo "docker compose"; \
  elif command -v docker-compose >/dev/null 2>&1; then \
    echo docker-compose; \
  elif command -v podman-compose >/dev/null 2>&1; then \
    echo podman-compose; \
  else \
    echo "docker compose"; \
  fi)

.PHONY: up down logs status \
        traffic-gen traffic-gen-zone1 traffic-gen-zone2 \
        failure-zone1 failure-zone2 failure-partial \
        sdk-test help

## help: Print this help
help:
	@grep -E '^##' $(MAKEFILE_LIST) | sed 's/## //'

# ── Stack ──────────────────────────────────────────────────────────────────────

## up: Build images and start the full stack (one command)
up:
	$(COMPOSE) up -d --build

## down: Stop and remove all containers and networks
down:
	$(COMPOSE) down

## logs: Follow logs from all running containers
logs:
	$(COMPOSE) logs -f

## status: Show container health status
status:
	$(COMPOSE) ps

# ── Tools (run via compose — no local Go or binary required) ──────────────────

## traffic-gen: 100 RPS to gateway for 30s across both zones
traffic-gen:
	$(COMPOSE) --profile tools run --rm traffic-gen \
	  -rps 100 -dur 30s -cabinets 10 -users 5

## traffic-gen-zone1: Pin 100 RPS to zone1 only
traffic-gen-zone1:
	$(COMPOSE) --profile tools run --rm traffic-gen \
	  -rps 100 -dur 30s -zone zone1

## traffic-gen-zone2: Pin 100 RPS to zone2 only
traffic-gen-zone2:
	$(COMPOSE) --profile tools run --rm traffic-gen \
	  -rps 100 -dur 30s -zone zone2

## failure-zone1: Simulate full zone1 outage, auto-restore after 30s
failure-zone1:
	$(COMPOSE) --profile tools run --rm failure-runner \
	  -restore-after 30s zone1-full

## failure-zone2: Simulate full zone2 outage, auto-restore after 30s
failure-zone2:
	$(COMPOSE) --profile tools run --rm failure-runner \
	  -restore-after 30s zone2-full

## failure-partial: Partial zone1 degradation (one container), restore after 30s
failure-partial:
	$(COMPOSE) --profile tools run --rm failure-runner \
	  -restore-after 30s zone1-partial

# ── Tests ──────────────────────────────────────────────────────────────────────

## sdk-test: Run SDK unit tests inside a container (no local Go required)
sdk-test:
	$(RUNTIME) run --rm \
	  -v "$(CURDIR)/sdk":/src/sdk \
	  golang:1.22 \
	  sh -c "cd /src/sdk && go test ./..."
