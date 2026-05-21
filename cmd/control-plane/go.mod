module github.com/geo-distributed-gateway/control-plane

go 1.26

// SDK replace is intentionally pre-wired even though T4 doesn't import it yet.
// T7 will publish sdk/config.RoutingHints via Redis Pub/Sub from this module;
// once T7 lands an import, `go mod tidy` will add the corresponding `require`
// and this `replace` will resolve to the local checkout.
replace github.com/geo-distributed-gateway/sdk => ../../sdk
