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
	slowAfter := flag.Duration("slow-after", 0, "slowdown start relative to process initialization")
	slowFor := flag.Duration("slow-for", 0, "slowdown duration; zero disables")
	slowDelay := flag.Duration("slow-delay", 200*time.Millisecond, "delay chosen during slowdown")
	maxAge := flag.Int("max-age", 10, "public item Cache-Control max-age in seconds; 0 makes items uncacheable")
	flag.Parse()
	if *delay < 0 || *slowAfter < 0 || *slowFor < 0 || *slowDelay < 0 {
		log.Fatal("delay must be nonnegative")
	}
	if *maxAge < 0 {
		log.Fatal("max-age must be nonnegative")
	}
	backend := mockbackend.NewWithSlowdown(*delay, *slowAfter, *slowFor, *slowDelay)
	backend.SetMaxAge(*maxAge)
	srv := &http.Server{Addr: *addr, Handler: backend, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("mock backend listening on %s (delay=%s, max-age=%ds)", *addr, *delay, *maxAge)
	log.Fatal(srv.ListenAndServe())
}
