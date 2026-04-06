FROM golang:1.22 AS builder
WORKDIR /src

COPY sdk/ ./sdk/
COPY cmd/traffic-gen/ ./cmd/traffic-gen/

RUN printf 'go 1.22\n\nuse (\n\t./cmd/traffic-gen\n\t./sdk\n)\n' > go.work && \
    touch go.work.sum

WORKDIR /src/cmd/traffic-gen
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/traffic-gen .

FROM scratch
COPY --from=builder /out/traffic-gen /traffic-gen
# Default: route to global-envoy service name inside compose network.
# Override with -url flag: docker compose run traffic-gen -url http://...
ENTRYPOINT ["/traffic-gen", "-url", "http://global-envoy:10000"]
