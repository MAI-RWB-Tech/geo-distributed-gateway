FROM docker.io/golang:1.22 AS builder
WORKDIR /src

# Copy only the modules needed for the app service.
# We create a minimal go.work that references only ./app and ./sdk
# (the root go.work also has cmd/* which are not needed here).
COPY sdk/ ./sdk/
COPY app/ ./app/

# Inline go.work so local replace resolves without external network.
RUN printf 'go 1.22\n\nuse (\n\t./app\n\t./sdk\n)\n' > go.work && \
    touch go.work.sum

WORKDIR /src/app
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /out/server ./cmd/service/main.go

FROM alpine:3.20
# wget is needed for healthcheck; no other runtime deps (static binary).
RUN apk add --no-cache wget
COPY --from=builder /out/server /server
EXPOSE 8080
ENV INSTANCE_NAME=unknown
ENV ZONE=unknown
ENTRYPOINT ["/server"]
