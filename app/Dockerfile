FROM golang:1.25 AS builder
WORKDIR /src

RUN apt-get update && \
    apt-get upgrade -y 

# кешируем модули
# COPY go.mod go.sum ./
# RUN go mod download

COPY . .

ARG INSTANCE
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/bin/service cmd/service/main.go

FROM scratch
COPY --from=builder /app/bin/service /server
EXPOSE 8080
ENV INSTANCE_NAME=unknown
ENTRYPOINT ["/server"]
