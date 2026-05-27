FROM golang:1.26 AS builder
WORKDIR /src

COPY sdk/ ./sdk/
COPY cmd/control-plane/ ./cmd/control-plane/

RUN printf 'go 1.26\n\nuse (\n\t./cmd/control-plane\n\t./sdk\n)\n' > go.work && \
    touch go.work.sum

WORKDIR /src/cmd/control-plane
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/control-plane .

FROM scratch
COPY --from=builder /out/control-plane /control-plane
ENTRYPOINT ["/control-plane"]
