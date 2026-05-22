FROM golang:1.26 AS builder
WORKDIR /src

COPY sdk/ ./sdk/
COPY cmd/ml-analyzer/ ./cmd/ml-analyzer/

RUN printf 'go 1.26\n\nuse (\n\t./cmd/ml-analyzer\n\t./sdk\n)\n' > go.work && \
    touch go.work.sum

WORKDIR /src/cmd/ml-analyzer
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ml-analyzer .

FROM alpine:3.20
RUN apk add --no-cache wget
COPY --from=builder /out/ml-analyzer /ml-analyzer
ENTRYPOINT ["/ml-analyzer"]
