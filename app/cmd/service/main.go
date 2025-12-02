package main

import (
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		inst := os.Getenv("INSTANCE_NAME")
		if inst == "" {
			inst = "unknown"
		}

		slog.Info("get /ping request received", slog.String("instance", inst))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf("pong: %s", inst)))
	})

	slog.Info("Starting server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}
