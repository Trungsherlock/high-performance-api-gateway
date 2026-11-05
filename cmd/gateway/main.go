package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"high-performance-api-gateway/internal/observe"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/gateway"
	"high-performance-api-gateway/internal/sharedcache"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listening address")
	upstream := flag.String("upstream", "http://127.0.0.1:9000", "fixed backend URL")
	timeout := flag.Duration("upstream-timeout", 10*time.Second, "total upstream request deadline")
	coalescing := flag.Bool("coalesce", true, "share eligible concurrent cache misses")
	maxConcurrent := flag.Int("max-concurrent", 32, "maximum active upstream requests; 0 disables limiter")
	maxWaiters := flag.Int("max-waiters", 32, "maximum queued upstream permit waiters; 0 rejects immediately, -1 selects legacy unbounded waiters")
	permitWait := flag.Duration("permit-wait", 50*time.Millisecond, "maximum wait for an upstream permit")
	adminAddr := flag.String("admin-addr", "127.0.0.1:8081", "dedicated numeric loopback admin listener; empty disables")
	instance := flag.String("instance", "gateway", "bounded instance identifier")
	logEvery := flag.Uint64("request-log-every", 1000, "sample one request log per N requests; 0 disables")
	cacheMode := flag.String("cache-mode", "local", "local, shared, or layered; CACHE_ENABLED=false disables all caching")
	redisAddr := flag.String("redis-addr", "127.0.0.1:6379", "optional shared-cache Redis address")
	redisTimeout := flag.Duration("redis-timeout", 50*time.Millisecond, "total deadline per shared-cache operation")
	redisPool := flag.Int("redis-pool", 16, "maximum shared-cache connections")
	leases := flag.Bool("leases", false, "coordinate eligible shared-cache fills with expiring Redis leases")
	leaseDuration := flag.Duration("lease-duration", 3*time.Second, "lease expiration; no renewal")
	leaseFill := flag.Duration("lease-fill-timeout", 2*time.Second, "owner deadline including recheck, backend and publication")
	leaseWait := flag.Duration("lease-wait", 4*time.Second, "maximum nonowner polling wait")
	leasePoll := flag.Duration("lease-poll", 25*time.Millisecond, "poll jitter base, up to twice this interval")
	degradation := flag.String("degradation", "protection-first", "lease-dependent misses: protection-first or available-local")
	// v2 became the default when V3.2 was accepted; v1 remains selectable for A/B runs.
	leaseProtocol := flag.String("lease-protocol", "v2", "v2: one read-or-acquire script, no release after an acknowledged publish; v1: read, SET NX, recheck, publish, release")
	cacheFlights := flag.Int("max-cache-flights", 64, "maximum registered distinct cache keys with local coalescing")
	cacheWaiters := flag.Int("max-cache-waiters", 1024, "maximum cache-miss clients including leaders; hits bypass admission")
	cacheEntries := flag.Int("cache-entries", 1000, "local (L1) cache entry limit")
	cacheBytes := flag.Int64("cache-bytes", 16<<20, "local (L1) cache body-byte limit")
	flag.Parse()
	if *degradation != "protection-first" && *degradation != "available-local" {
		log.Fatal("invalid degradation policy")
	}
	if *leaseProtocol != "v1" && *leaseProtocol != "v2" {
		log.Fatal("invalid lease protocol")
	}
	if *cacheFlights < 1 || *cacheWaiters < 1 {
		log.Fatal("cache admission limits must be positive")
	}
	if *cacheEntries < 1 || *cacheBytes < 1 {
		log.Fatal("cache entry and byte limits must be positive")
	}
	if *cacheMode != "local" && *cacheMode != "shared" && *cacheMode != "layered" {
		log.Fatal("invalid cache-mode")
	}
	if len(*instance) > 64 {
		log.Fatal("instance must be at most 64 bytes")
	}
	var adminListener net.Listener
	if *adminAddr != "" {
		host, _, err := net.SplitHostPort(*adminAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			log.Fatal("admin listener must use a numeric loopback address")
		}
		adminListener, err = net.Listen("tcp", *adminAddr)
		if err != nil {
			log.Fatal(err)
		}
		defer adminListener.Close()
	}
	metrics := observe.New(*instance, os.Stderr, *logEvery)
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
	if enabled && *cacheMode != "shared" {
		storage = cache.New(*cacheEntries, *cacheBytes, nil)
	}
	var shared sharedcache.Store
	if enabled && *cacheMode != "local" {
		adapter, err := sharedcache.New(*redisAddr, *redisTimeout, *redisPool)
		if err != nil {
			log.Fatal(err)
		}
		defer adapter.Close()
		shared = adapter
	}
	var lease *gateway.LeaseOptions
	if *leases {
		lease = &gateway.LeaseOptions{Degradation: *degradation, Protocol: *leaseProtocol, Duration: *leaseDuration, FillTimeout: *leaseFill, WaitTimeout: *leaseWait, PollInterval: *leasePoll, Margin: 250 * time.Millisecond}
	}
	var cacheAdmission *gateway.CacheAdmission
	if enabled && *coalescing {
		cacheAdmission = &gateway.CacheAdmission{MaxFlights: *cacheFlights, MaxWaiters: *cacheWaiters}
	}
	proxy, transport, err := gateway.NewWithOptions(*upstream, *timeout, storage, gateway.Options{CacheAdmission: cacheAdmission, Shared: shared, Lease: lease, Metrics: metrics, Coalesce: *coalescing, MaxConcurrent: *maxConcurrent, PermitWait: *permitWait, MaxWaiters: maxWaiters, Lifecycle: lifecycle})
	if err != nil {
		log.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if storage != nil {
		metrics.CacheStats = storage.Stats
	}
	var admin *http.Server
	if adminListener != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "ready: local state initialized; backend and optional Redis health not checked")
		})
		admin = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
		defer admin.Close()
	}
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
	if admin != nil {
		go func() { serverErrors <- admin.Serve(adminListener) }()
	}
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
