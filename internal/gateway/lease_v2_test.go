package gateway

import (
	"context"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/observe"
	"high-performance-api-gateway/internal/sharedcache"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func dependencyOps(m *observe.Metrics) uint64 {
	return m.Snapshot()["histograms"].(map[string]observe.Histogram)["cache_dependency"].Count
}

// v1 pays read, SET NX, recheck, publish and a no-op release; v2 pays one
// read-or-acquire script and the publish, which already deleted the lease.
func TestLeaseOperationsPerFill(t *testing.T) {
	forProtocols(t, testLeaseOperationsPerFill)
}
func testLeaseOperationsPerFill(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "filled")
	}))
	defer backend.Close()
	var metrics []*observe.Metrics
	for i := 0; i < 2; i++ {
		m := observe.New("test", nil, 0)
		h, tr, err := NewWithOptions(backend.URL, 3*time.Second, cache.New(10, 1<<20, nil), Options{Shared: shared, Lease: leaseSettingsFor(protocol), Coalesce: true, Metrics: m})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
		if w.Code != 200 || w.Body.String() != "filled" {
			t.Fatal(i, w.Code, w.Body.String())
		}
		metrics = append(metrics, m)
	}
	want := map[string]uint64{"v1": 5, "v2": 2}[protocol]
	owner := metrics[0].Snapshot()["counters"].(map[string]int64)
	if got := dependencyOps(metrics[0]); got != want || calls.Load() != 1 || owner["lease_publications"] != 1 || owner["lease_releases"] != 0 {
		t.Fatal("owner operations", got, want, calls.Load(), owner)
	}
	// The second gateway has an empty L1, so it reads the published value: one operation.
	reader := metrics[1].Snapshot()["counters"].(map[string]int64)
	if got := dependencyOps(metrics[1]); got != 1 || reader["shared_hits"] != 1 || reader["lease_acquired"] != 0 {
		t.Fatal("reader operations", got, reader)
	}
}

func TestLeaseV2CorruptValueRefilledWithoutStall(t *testing.T) {
	shared := realCoordinator(t)
	entered := make(chan struct{}, 1)
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
		io.WriteString(w, "fresh")
	}))
	defer backend.Close()
	raw := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_TEST_ADDR")})
	defer raw.Close()
	key := sharedcache.Key(backend.URL + `/api/public/item|""|""|""`)
	if err := raw.Set(context.Background(), key, "not-json", 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	var handlers []http.Handler
	var metrics []*observe.Metrics
	for i := 0; i < 2; i++ {
		m := observe.New("test", nil, 0)
		o := leaseSettingsFor("v2")
		o.Duration = 3 * time.Second
		o.FillTimeout = 2 * time.Second
		h, tr, err := NewWithOptions(backend.URL, 4*time.Second, cache.New(10, 1<<20, nil), Options{Shared: shared, Lease: o, Coalesce: true, Metrics: m})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		handlers = append(handlers, h)
		metrics = append(metrics, m)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			handlers[i%2].ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
			if w.Code != 200 || w.Body.String() != "fresh" {
				t.Errorf("%d %s", w.Code, w.Body.String())
			}
		}(i)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no owner replaced the corrupt value")
	}
	waitCondition(t, func() bool {
		return metrics[0].Snapshot()["counters"].(map[string]int64)["distributed_followers"]+metrics[1].Snapshot()["counters"].(map[string]int64)["distributed_followers"] == 1
	})
	released := time.Now()
	once.Do(func() { close(release) })
	wg.Wait()
	// The waiter reads the replacement on its next poll, well before the 2s wait bound.
	if elapsed := time.Since(released); elapsed > 500*time.Millisecond || calls.Load() != 1 {
		t.Fatal("stalled or refetched", elapsed, calls.Load())
	}
	for _, m := range metrics {
		c := m.Snapshot()["counters"].(map[string]int64)
		if c["lease_wait_timeouts"] != 0 || c["dependency_errors"] != 0 {
			t.Fatal(c)
		}
		for k, v := range m.Snapshot()["gauges"].(map[string]int64) {
			if v != 0 {
				t.Fatal(k, v)
			}
		}
	}
}

// An owner that dies without releasing blocks other owners only until its lease expires.
func TestLeaseRecoversAfterAbandonedLease(t *testing.T) {
	forProtocols(t, testLeaseRecoversAfterAbandonedLease)
}
func testLeaseRecoversAfterAbandonedLease(t *testing.T, protocol string) {
	shared := realCoordinator(t)
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "recovered")
	}))
	defer backend.Close()
	if _, owned, err := shared.Acquire(context.Background(), backend.URL+`/api/public/item|""|""|""`, 150*time.Millisecond); err != nil || !owned {
		t.Fatal(owned, err)
	}
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions(backend.URL, 3*time.Second, nil, Options{Shared: shared, Lease: leaseSettingsFor(protocol), Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	elapsed := time.Since(start)
	c := m.Snapshot()["counters"].(map[string]int64)
	if w.Code != 200 || w.Body.String() != "recovered" || calls.Load() != 1 || elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatal(w.Code, w.Body.String(), calls.Load(), elapsed)
	}
	if c["distributed_followers"] != 1 || c["lease_acquired"] != 1 || c["lease_wait_timeouts"] != 0 {
		t.Fatal(c)
	}
}

func TestLeaseProtocolValidation(t *testing.T) {
	// completedBeforeAcquire embeds a v1-only Coordinator, so it lacks ReadOrAcquire.
	v1Only := completedBeforeAcquire{}
	if _, _, err := NewWithOptions("http://localhost", time.Second, nil, Options{Shared: v1Only, Lease: leaseSettingsFor("v1"), Coalesce: true}); err != nil {
		t.Fatal("v1 coordinator rejected", err)
	}
	if _, _, err := NewWithOptions("http://localhost", time.Second, nil, Options{Shared: v1Only, Lease: leaseSettingsFor("v2"), Coalesce: true}); err == nil {
		t.Fatal("v2 accepted a coordinator without read-or-acquire")
	}
	if leaseSettingsFor("v3").valid() {
		t.Fatal("unknown protocol accepted")
	}
}
