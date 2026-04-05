FROM golang:1.22 AS builder
WORKDIR /src

COPY sdk/ ./sdk/
COPY cmd/failure-runner/ ./cmd/failure-runner/

RUN printf 'go 1.22\n\nuse (\n\t./cmd/failure-runner\n\t./sdk\n)\n' > go.work && \
    touch go.work.sum

WORKDIR /src/cmd/failure-runner
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/failure-runner .

# docker:cli provides the docker binary — works with both Docker socket
# and Podman socket (via DOCKER_HOST env var).
FROM docker:27-cli
COPY --from=builder /out/failure-runner /usr/local/bin/failure-runner

# Default probe URL uses compose service name; socket is mounted at runtime.
# docker compose run failure-runner zone1-full
ENTRYPOINT ["failure-runner", \
  "-runtime",   "docker", \
  "-probe-url", "http://global-envoy:10000/ping", \
  "-admin1",    "http://zone1-envoy:9901", \
  "-admin2",    "http://zone2-envoy:9901"]
