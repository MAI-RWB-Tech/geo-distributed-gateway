module github.com/geo-distributed-gateway/app

go 1.26

require (
	github.com/geo-distributed-gateway/sdk v0.0.0
	github.com/hashicorp/consul/api v1.32.1
)

replace github.com/geo-distributed-gateway/sdk => ../sdk
