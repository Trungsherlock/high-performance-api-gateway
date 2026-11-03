package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"high-performance-api-gateway/internal/front"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8082", "front data listener")
	admin := flag.String("admin-addr", "127.0.0.1:8083", "numeric loopback administration listener")
	urls := flag.String("targets", "http://127.0.0.1:8080,http://127.0.0.1:8090", "comma-separated gateway origins")
	health := flag.String("readiness", "http://127.0.0.1:8081/readyz,http://127.0.0.1:8091/readyz", "matching readiness URLs")
	timeout := flag.Duration("timeout", 12*time.Second, "whole forwarding deadline")
	interval := flag.Duration("probe-interval", 100*time.Millisecond, "health probe interval")
	probeTimeout := flag.Duration("probe-timeout", 100*time.Millisecond, "per-target health deadline")
	flag.Parse()
	if *interval <= 0 {
		log.Fatal("probe interval must be positive")
	}
	host, _, err := net.SplitHostPort(*admin)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		log.Fatal("admin requires a numeric loopback address")
	}
	origins, checks := strings.Split(*urls, ","), strings.Split(*health, ",")
	if len(origins) != len(checks) {
		log.Fatal("targets and readiness lists must match")
	}
	targets := make([]front.Target, len(origins))
	for i := range origins {
		targets[i] = front.Target{Name: fmt.Sprintf("gateway-%d", i+1), URL: strings.TrimSpace(origins[i]), ReadinessURL: strings.TrimSpace(checks[i])}
	}
	p, err := front.New(targets, *timeout, *probeTimeout)
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p.Check(ctx)
	go p.RunHealthChecks(ctx, *interval)
	server := &http.Server{Addr: *addr, Handler: p, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: *timeout + time.Second, IdleTimeout: 60 * time.Second}
	control := &http.Server{Addr: *admin, Handler: p.Admin(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- server.ListenAndServe() }()
	go func() { errs <- control.ListenAndServe() }()
	log.Printf("front listening on %s, admin=%s, targets=%d", *addr, *admin, len(targets))
	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
			stop()
			server.Close()
			control.Close()
			os.Exit(1)
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
	}
	control.Close()
}
