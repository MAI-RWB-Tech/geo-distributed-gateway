FROM golang:1.26 AS builder
WORKDIR /src

COPY sdk/ ./sdk/
COPY cmd/events-collector/ ./cmd/events-collector/

RUN printf 'go 1.26\n\nuse (\n\t./cmd/events-collector\n\t./sdk\n)\n' > go.work && \
    touch go.work.sum

WORKDIR /src/cmd/events-collector
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/events-collector .

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates
COPY --from=builder /out/events-collector /events-collector
EXPOSE 9100
ENTRYPOINT ["/events-collector"]
