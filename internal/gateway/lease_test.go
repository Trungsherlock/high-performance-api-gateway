package gateway

import (
	"context"
	"fmt"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/observe"
	"high-performance-api-gateway/internal/sharedcache"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func leaseSettings() *LeaseOptions {
	return &LeaseOptions{Duration: time.Second, FillTimeout: 700 * time.Millisecond, WaitTimeout: 2 * time.Second, PollInterval: 5 * time.Millisecond, Margin: 100 * time.Millisecond}
}
func leaseSettingsFor(protocol string) *LeaseOptions {
	o := leaseSettings()
	o.Protocol = protocol
	return o
}

// forProtocols runs a lease test under both protocols, which must behave alike.
func forProtocols(t *testing.T, test func(*testing.T, string)) {
	for _, protocol := range []string{"v1", "v2"} {
		t.Run(protocol, func(t *testing.T) { test(t, protocol) })
	}
}
func realCoordinator(t *testing.T) *sharedcache.Redis {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR")
	}
	r, err := sharedcache.New(addr, 200*time.Millisecond, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func waitCondition(t *testing.T, fn func() bool) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition deadline")
}
func TestLeaseBurstAcrossGateways(t *testing.T) {
	forProtocols(t, testLeaseBurstAcrossGateways)
}
func testLeaseBurstAcrossGateways(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "version-1")
	}))
	defer backend.Close()
	var handlers []http.Handler
	var metrics []*observe.Metrics
	for i := 0; i < 2; i++ {
		m := observe.New(fmt.Sprint(i), nil, 0)
		o := leaseSettingsFor(protocol)
		o.FillTimeout = 2 * time.Second
		o.Duration = 3 * time.Second
		h, tr, err := NewWithOptions(backend.URL, 4*time.Second, cache.New(10, 1<<20, nil), Options{Shared: shared, Lease: o, Coalesce: true, Metrics: m})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		handlers = append(handlers, h)
		metrics = append(metrics, m)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			handlers[i%2].ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
			if w.Code != 200 || w.Body.String() != "version-1" {
				t.Errorf("%d %s", w.Code, w.Body.String())
			}
		}(i)
	}
	close(start)
	waitCondition(t, func() bool {
		return metrics[0].Snapshot()["counters"].(map[string]int64)["local_followers"]+metrics[1].Snapshot()["counters"].(map[string]int64)["local_followers"] == 98
	})
	waitCondition(t, func() bool {
		return metrics[0].Snapshot()["counters"].(map[string]int64)["distributed_followers"]+metrics[1].Snapshot()["counters"].(map[string]int64)["distributed_followers"] >= 1
	})
	once.Do(func() { close(release) })
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("backend fills", calls.Load())
	}
	for _, m := range metrics {
		for k, v := range m.Snapshot()["gauges"].(map[string]int64) {
			if v != 0 {
				t.Fatal(k, v)
			}
		}
	}
}

type replaceOwner struct{ sharedcache.AtomicCoordinator }

func (s replaceOwner) Publish(ctx context.Context, key, token string, v cache.Response) (bool, error) {
	if _, err := s.AtomicCoordinator.Release(ctx, key, token); err != nil {
		return false, err
	}
	next, ok, err := s.AtomicCoordinator.Acquire(ctx, key, time.Second)
	if err != nil || !ok {
		return false, fmt.Errorf("replacement acquisition: %v", err)
	}
	newer := v
	newer.Body = []byte("new-version")
	if ok, err := s.AtomicCoordinator.Publish(ctx, key, next, newer); err != nil || !ok {
		return false, fmt.Errorf("replacement publication: %v", err)
	}
	return s.AtomicCoordinator.Publish(ctx, key, token, v)
}
func TestRejectedLeasePublicationNeverWarmsLocal(t *testing.T) {
	forProtocols(t, testRejectedLeasePublicationNeverWarmsLocal)
}
func testRejectedLeasePublicationNeverWarmsLocal(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "old-version")
	}))
	defer backend.Close()
	local := cache.New(10, 1<<20, nil)
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions(backend.URL, 3*time.Second, local, Options{Shared: replaceOwner{shared}, Lease: leaseSettingsFor(protocol), Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 503 || w.Header().Get("X-Gateway-Rejection") != "lease_lost" {
		t.Fatal(w.Code, w.Header())
	}
	if n, _ := local.Stats(); n != 0 {
		t.Fatal("rejected result warmed L1", n)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 200 || w.Body.String() != "new-version" || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
}

// A nonowner has no authority to issue the speculative backend fill.
type contendedCoordinator struct{ sharedcache.Store }

func (contendedCoordinator) Acquire(context.Context, string, time.Duration) (string, bool, error) {
	return "", false, nil
}
func (contendedCoordinator) Release(context.Context, string, string) (bool, error) { return false, nil }

// ReadOrAcquire fails like the store's read would, and otherwise always finds another owner.
func (s contendedCoordinator) ReadOrAcquire(ctx context.Context, key string, _ time.Duration, _ bool) (sharedcache.Claim, cache.Response, string, error) {
	_, _, err := s.Store.Get(ctx, key)
	return sharedcache.ClaimWait, cache.Response{}, "", err
}
func (contendedCoordinator) Publish(context.Context, string, string, cache.Response) (bool, error) {
	panic("nonowner published")
}

type emptyStore struct{}

func (emptyStore) Get(context.Context, string) (cache.Response, bool, error) {
	return cache.Response{}, false, nil
}
func (emptyStore) Put(context.Context, string, cache.Response) error { return nil }
func TestLeaseWaitBoundAndFailure(t *testing.T) {
	forProtocols(t, testLeaseWaitBoundAndFailure)
}
func testLeaseWaitBoundAndFailure(t *testing.T, protocol string) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			var store sharedcache.Store = emptyStore{}
			if failure {
				store = unavailableStore{}
			}
			m := observe.New("test", nil, 0)
			o := leaseSettingsFor(protocol)
			o.WaitTimeout = 30 * time.Millisecond
			h, tr, err := NewWithOptions("http://127.0.0.1:1", time.Second, nil, Options{Shared: contendedCoordinator{store}, Lease: o, Coalesce: true, Metrics: m})
			if err != nil {
				t.Fatal(err)
			}
			defer tr.CloseIdleConnections()
			start := time.Now()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
			if w.Code != 503 || time.Since(start) > time.Second {
				t.Fatal(w.Code, time.Since(start))
			}
			c := m.Snapshot()["counters"].(map[string]int64)
			if c["backend_attempts"] != 0 {
				t.Fatal(c)
			}
			if !failure && (w.Header().Get("X-Gateway-Rejection") != "coordination_timeout" || c["lease_wait_timeouts"] != 1) {
				t.Fatal(w.Header(), c)
			}
			for k, v := range m.Snapshot()["gauges"].(map[string]int64) {
				if v != 0 {
					t.Fatal(k, v)
				}
			}
		})
	}
}
func TestLeaseCallerCancellationDoesNotCancelOtherGateway(t *testing.T) {
	forProtocols(t, testLeaseCallerCancellationDoesNotCancelOtherGateway)
}
func testLeaseCallerCancellationDoesNotCancelOtherGateway(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	var handlers []http.Handler
	var metrics []*observe.Metrics
	for i := 0; i < 2; i++ {
		m := observe.New("test", nil, 0)
		o := leaseSettingsFor(protocol)
		o.Duration = 3 * time.Second
		o.FillTimeout = 2 * time.Second
		h, tr, err := NewWithOptions(backend.URL, 4*time.Second, nil, Options{Shared: shared, Lease: o, Coalesce: true, Metrics: m})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		handlers = append(handlers, h)
		metrics = append(metrics, m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aDone := make(chan struct{})
	bDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		defer close(aDone)
		handlers[0].ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://gateway/api/public/item", nil).WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner did not reach backend")
	}
	go func() {
		w := httptest.NewRecorder()
		handlers[1].ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
		bDone <- w
	}()
	waitCondition(t, func() bool { return metrics[1].Snapshot()["counters"].(map[string]int64)["distributed_followers"] == 1 })
	cancel()
	<-aDone
	once.Do(func() { close(release) })
	select {
	case w := <-bDone:
		if w.Code != 200 || w.Body.String() != "ok" || calls.Load() != 1 {
			t.Fatal(w.Code, w.Body.String(), calls.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("healthy waiter stalled")
	}
}
func TestInvalidLeaseOptions(t *testing.T) {
	if _, _, err := NewWithOptions("http://localhost", time.Second, nil, Options{Lease: leaseSettings(), Coalesce: true}); err == nil {
		t.Fatal("missing coordinator accepted")
	}
	o := leaseSettings()
	o.FillTimeout = o.Duration
	if o.valid() {
		t.Fatal("fill may outlive lease")
	}
}

func TestLeaseUnshareableStreamingFallback(t *testing.T) {
	forProtocols(t, testLeaseUnshareableStreamingFallback)
}
func testLeaseUnshareableStreamingFallback(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	for _, kind := range []string{"private", "large"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if kind == "private" {
					w.Header().Set("Cache-Control", "private, max-age=10")
					io.WriteString(w, "private")
				} else {
					w.Header().Set("Cache-Control", "public, max-age=10")
					io.WriteString(w, strings.Repeat("x", 65537))
				}
			}))
			defer backend.Close()
			local := cache.New(10, 1<<20, nil)
			m := observe.New("test", nil, 0)
			h, tr, err := NewWithOptions(backend.URL, 3*time.Second, local, Options{Shared: shared, Lease: leaseSettingsFor(protocol), Coalesce: true, Metrics: m})
			if err != nil {
				t.Fatal(err)
			}
			defer tr.CloseIdleConnections()
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
				if w.Code != 200 {
					t.Fatal(w.Code)
				}
				if kind == "large" && w.Body.Len() != 65537 {
					t.Fatal("stream truncated", w.Body.Len())
				}
			}
			if calls.Load() != 4 {
				t.Fatal("expected one probe and individual fetch per call", calls.Load())
			}
			if n, _ := local.Stats(); n != 0 {
				t.Fatal("unshareable cached")
			}
			c := m.Snapshot()["counters"].(map[string]int64)
			if c["lease_publications"] != 0 || c["lease_releases"] != 2 {
				t.Fatal(c)
			}
		})
	}
}
func TestLeaseOwnerTimeoutReleasesForNextRequest(t *testing.T) {
	forProtocols(t, testLeaseOwnerTimeoutReleasesForNextRequest)
}
func testLeaseOwnerTimeoutReleasesForNextRequest(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "recovered")
	}))
	defer backend.Close()
	o := leaseSettingsFor(protocol)
	o.FillTimeout = 50 * time.Millisecond
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions(backend.URL, 3*time.Second, nil, Options{Shared: shared, Lease: o, Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 504 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 200 || w.Body.String() != "recovered" {
		t.Fatal(w.Code, w.Body.String())
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	if calls.Load() != 2 || c["lease_publications"] != 1 || c["lease_releases"] != 1 {
		t.Fatal(calls.Load(), c)
	}
	for k, v := range m.Snapshot()["gauges"].(map[string]int64) {
		if v != 0 {
			t.Fatal(k, v)
		}
	}
}

type completedBeforeAcquire struct{ sharedcache.Coordinator }

func (s completedBeforeAcquire) Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	v := cache.Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}}, Body: []byte("already-complete"), ExpiresAt: time.Now().Add(time.Second)}
	if err := s.Coordinator.Put(ctx, key, v); err != nil {
		return "", false, err
	}
	return s.Coordinator.Acquire(ctx, key, ttl)
}
func TestLeaseRechecksAfterAcquiring(t *testing.T) {
	shared := realCoordinator(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redundant backend fill"); w.WriteHeader(500) }))
	defer backend.Close()
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions(backend.URL, 3*time.Second, nil, Options{Shared: completedBeforeAcquire{shared}, Lease: leaseSettings(), Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 200 || w.Body.String() != "already-complete" {
		t.Fatal(w.Code, w.Body.String())
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	if c["backend_attempts"] != 0 || c["lease_releases"] != 1 {
		t.Fatal(c)
	}
}
func TestLeaseFreshLocalHitSurvivesDependencyFailure(t *testing.T) {
	forProtocols(t, testLeaseFreshLocalHitSurvivesDependencyFailure)
}
func testLeaseFreshLocalHitSurvivesDependencyFailure(t *testing.T, protocol string) {
	local := cache.New(10, 1<<20, nil)
	local.Put(`http://127.0.0.1:1/api/public/item|""|""|""`, cache.Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}}, Body: []byte("fresh"), ExpiresAt: time.Now().Add(time.Second)})
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions("http://127.0.0.1:1", time.Second, local, Options{Shared: contendedCoordinator{unavailableStore{}}, Lease: leaseSettingsFor(protocol), Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 200 || w.Body.String() != "fresh" {
		t.Fatal(w.Code, w.Body.String())
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	if c["dependency_errors"] != 0 || c["cache_hits"] != 1 || c["lease_acquired"] != 0 {
		t.Fatal(c)
	}
}

type expiringAcquire struct{ contendedCoordinator }

func (expiringAcquire) Acquire(ctx context.Context, _ string, _ time.Duration) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}
func (expiringAcquire) ReadOrAcquire(ctx context.Context, _ string, _ time.Duration, _ bool) (sharedcache.Claim, cache.Response, string, error) {
	<-ctx.Done()
	return sharedcache.ClaimWait, cache.Response{}, "", ctx.Err()
}
func TestLeaseAcquisitionDeadlineReason(t *testing.T) {
	forProtocols(t, testLeaseAcquisitionDeadlineReason)
}
func testLeaseAcquisitionDeadlineReason(t *testing.T, protocol string) {
	m := observe.New("test", nil, 0)
	o := leaseSettingsFor(protocol)
	o.WaitTimeout = 20 * time.Millisecond
	h, tr, err := NewWithOptions("http://127.0.0.1:1", time.Second, nil, Options{Shared: expiringAcquire{contendedCoordinator{emptyStore{}}}, Lease: o, Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 503 || w.Header().Get("X-Gateway-Rejection") != "coordination_timeout" {
		t.Fatal(w.Code, w.Header())
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	if c["lease_wait_timeouts"] != 1 || c["dependency_errors"] != 1 || c["dependency_error_timeout"] != 1 || !causesReconcile(c) || c["backend_attempts"] != 0 {
		t.Fatal(c)
	}
}
