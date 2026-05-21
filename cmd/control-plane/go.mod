module github.com/geo-distributed-gateway/control-plane

go 1.26

require (
	github.com/geo-distributed-gateway/sdk v0.0.0
	github.com/redis/go-redis/v9 v9.7.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
)

// T7 publishes sdk/config.RoutingHints to Redis Pub/Sub from this module,
// so the SDK is resolved from the local checkout.
replace github.com/geo-distributed-gateway/sdk => ../../sdk
