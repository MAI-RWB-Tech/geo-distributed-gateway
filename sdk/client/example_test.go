package client_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/geo-distributed-gateway/sdk/client"
)

func Example() {
	// Spin up a tiny test server to stand in for Envoy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok user=%s cabinet=%s corr=%s",
			r.Header.Get("X-User-ID"),
			r.Header.Get("X-Cabinet-ID"),
			r.Header.Get("X-Correlation-ID"),
		)
	}))
	defer srv.Close()

	c := client.New(client.Options{BaseURL: srv.URL})
	result := c.Do(context.Background(), "/ping", "user-42", "cabinet-7")

	fmt.Printf("status=%d latency>0=%v err=%v\n", result.StatusCode, result.Latency > 0, result.Err)
	// Output: status=200 latency>0=true err=<nil>
}
