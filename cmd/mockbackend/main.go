package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"high-performance-api-gateway/internal/mockbackend"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listening address")
	delay := flag.Duration("delay", 50*time.Millisecond, "artificial item response delay")
	flag.Parse()
	if *delay < 0 {
		log.Fatal("delay must be nonnegative")
	}
	srv := &http.Server{Addr: *addr, Handler: mockbackend.New(*delay), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("mock backend listening on %s (delay=%s)", *addr, *delay)
	log.Fatal(srv.ListenAndServe())
}
