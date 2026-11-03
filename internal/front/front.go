// Package front selects one healthy gateway per request. It never reroutes failures.
package front

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Target struct{ Name, URL, ReadinessURL string }
type TargetStats struct {
	Name                                    string
	Healthy                                 bool
	Selected, Errors, Probes, ProbeFailures uint64
}
type Stats struct {
	Requests, Completed, Unavailable, Aborted uint64
	Active                                    int
	Targets                                   []TargetStats
}
type endpoint struct {
	config Target
	url    *url.URL
	stats  TargetStats
}
type selection struct{ index int }
type Proxy struct {
	mu                         sync.Mutex
	probeMu                    sync.Mutex
	targets                    []endpoint
	next                       int
	stats                      Stats
	proxy                      *httputil.ReverseProxy
	transport, healthTransport *http.Transport
	healthClient               *http.Client
	timeout                    time.Duration
}

func parse(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("expected HTTP(S) URL without credentials, query, or fragment")
	}
	return u, nil
}
func New(targets []Target, timeout, probeTimeout time.Duration) (*Proxy, error) {
	if len(targets) == 0 || len(targets) > 16 || timeout <= 0 || probeTimeout <= 0 {
		return nil, fmt.Errorf("need 1-16 targets and positive deadlines")
	}
	p := &Proxy{timeout: timeout}
	names := map[string]bool{}
	for _, config := range targets {
		u, err := parse(config.URL)
		if err != nil || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("target must be an HTTP(S) origin")
		}
		if _, err := parse(config.ReadinessURL); err != nil {
			return nil, fmt.Errorf("invalid readiness URL")
		}
		if config.Name == "" || len(config.Name) > 64 || names[config.Name] {
			return nil, fmt.Errorf("target names must be unique and 1-64 bytes")
		}
		names[config.Name] = true
		p.targets = append(p.targets, endpoint{config: config, url: u, stats: TargetStats{Name: config.Name}})
	}
	p.transport = &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, DisableCompression: true, MaxConnsPerHost: 256, MaxIdleConnsPerHost: 32, MaxIdleConns: 128, IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: timeout, TLSHandshakeTimeout: time.Second}
	p.healthTransport = &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second}
	p.healthClient = &http.Client{Transport: p.healthTransport, Timeout: probeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p.proxy = &httputil.ReverseProxy{Transport: p.transport, BufferPool: &buffers{}, Rewrite: func(r *httputil.ProxyRequest) {
		chosen := r.In.Context().Value(selection{}).(int)
		r.SetURL(p.targets[chosen].url)
		r.Out.URL.RawQuery = r.In.URL.RawQuery
		// SetXForwarded is deliberately omitted: extra representation headers bypass
		// the gateway's narrow public cache. Rewrite strips untrusted forwarding data.
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		chosen := r.Context().Value(selection{}).(int)
		p.mu.Lock()
		p.targets[chosen].stats.Errors++
		// Independent readiness probes own health; caller cancellation is not a health signal.
		p.mu.Unlock()
		code := http.StatusBadGateway
		var e net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &e) && e.Timeout()) {
			code = http.StatusGatewayTimeout
		}
		http.Error(w, http.StatusText(code), code)
	}}
	return p, nil
}
func (p *Proxy) Check(ctx context.Context) {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	var wg sync.WaitGroup
	for i := range p.targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, "GET", p.targets[i].config.ReadinessURL, nil)
			healthy := false
			if err == nil {
				resp, err := p.healthClient.Do(req)
				if err == nil {
					_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
					resp.Body.Close()
					healthy = resp.StatusCode == 200 && readErr == nil
				}
			}
			p.mu.Lock()
			s := &p.targets[i].stats
			s.Probes++
			s.Healthy = healthy
			if !healthy {
				s.ProbeFailures++
			}
			p.mu.Unlock()
		}(i)
	}
	wg.Wait()
}
func (p *Proxy) RunHealthChecks(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Check(ctx)
		}
	}
}
func (p *Proxy) Snapshot() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Targets = make([]TargetStats, len(p.targets))
	for i := range p.targets {
		s.Targets[i] = p.targets[i].stats
	}
	return s
}
func (p *Proxy) Close() { p.transport.CloseIdleConnections(); p.healthTransport.CloseIdleConnections() }
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.stats.Requests++
	p.stats.Active++
	chosen := -1
	for n := 0; n < len(p.targets); n++ {
		i := (p.next + n) % len(p.targets)
		if p.targets[i].stats.Healthy {
			chosen = i
			p.next = (i + 1) % len(p.targets)
			p.targets[i].stats.Selected++
			break
		}
	}
	if chosen < 0 {
		p.stats.Unavailable++
	}
	p.mu.Unlock()
	defer func() {
		failure := recover()
		p.mu.Lock()
		p.stats.Active--
		p.stats.Completed++
		if failure != nil {
			p.stats.Aborted++
		}
		p.mu.Unlock()
		if failure != nil {
			panic(failure)
		}
	}()
	if chosen < 0 {
		http.Error(w, "no healthy gateways", 503)
		return
	}
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" {
		http.Error(w, "tunneling and upgrades unsupported", 501)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), selection{}, chosen), p.timeout)
	defer cancel()
	p.proxy.ServeHTTP(w, r.WithContext(ctx))
}
func (p *Proxy) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		s := p.Snapshot()
		for _, t := range s.Targets {
			if t.Healthy {
				io.WriteString(w, "ready\n")
				return
			}
		}
		http.Error(w, "no healthy gateways", 503)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.Snapshot())
	})
	return mux
}

type buffers struct{ pool sync.Pool }

func (b *buffers) Get() []byte {
	if v := b.pool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, 32<<10)
}
func (b *buffers) Put(v []byte) {
	if cap(v) == 32<<10 {
		b.pool.Put(v[:32<<10])
	}
}
