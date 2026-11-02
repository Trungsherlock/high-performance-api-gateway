package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/gateway"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listening address")
	upstream := flag.String("upstream", "http://127.0.0.1:9000", "fixed backend URL")
	timeout := flag.Duration("upstream-timeout", 10*time.Second, "total upstream request deadline")
	coalescing := flag.Bool("coalesce", true, "share eligible concurrent cache misses")
	maxConcurrent := flag.Int("max-concurrent", 32, "maximum active upstream requests; 0 disables limiter")
	permitWait := flag.Duration("permit-wait", 50*time.Millisecond, "maximum wait for an upstream permit")
	flag.Parse()
	// Shared work survives individual clients but is canceled after server shutdown.
	lifecycle, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	enabled := true
	if value := os.Getenv("CACHE_ENABLED"); value != "" {
		var err error
		enabled, err = strconv.ParseBool(value)
		if err != nil {
			log.Fatal("CACHE_ENABLED must be true or false")
		}
	}
	var storage *cache.Cache
	if enabled {
		storage = cache.New(1000, 16<<20, nil)
	}
	proxy, transport, err := gateway.NewWithOptions(*upstream, *timeout, storage, gateway.Options{Coalesce: *coalescing, MaxConcurrent: *maxConcurrent, PermitWait: *permitWait, Lifecycle: lifecycle})
	if err != nil {
		log.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	// Avoid ServeMux path cleaning and redirects for proxied requests.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
			return
		}
		proxy.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: *timeout + 15*time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- srv.ListenAndServe() }()
	log.Printf("gateway listening on %s (upstream=%s, cache=%t)", *addr, *upstream, enabled)
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		stop()
		shutdown(srv, 5*time.Second)
	}
}

func shutdown(srv *http.Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown: %v; closing active connections", err)
		_ = srv.Close()
	}
}
