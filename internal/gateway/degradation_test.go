package gateway

import (
	"context"
	"errors"
	"fmt"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/observe"
	"high-performance-api-gateway/internal/sharedcache"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDegradationPolicies(t *testing.T) {
	for _, policy := range []string{"protection-first", "available-local"} {
		for _, fault := range []string{"unavailable", "wait-timeout"} {
			t.Run(policy+"/"+fault, func(t *testing.T) {
				var calls atomic.Int64
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Cache-Control", "public, max-age=10")
					io.WriteString(w, "ok")
				}))
				defer backend.Close()
				var store sharedcache.Store = unavailableStore{}
				if fault == "wait-timeout" {
					store = emptyStore{}
				}
				local := cache.New(10, 1<<20, nil)
				m := observe.New("test", nil, 0)
				o := leaseSettings()
				o.Degradation = policy
				o.WaitTimeout = 25 * time.Millisecond
				h, tr, err := NewWithOptions(backend.URL, time.Second, local, Options{Shared: contendedCoordinator{store}, Lease: o, Coalesce: true, Metrics: m, MaxConcurrent: 1, CacheAdmission: &CacheAdmission{MaxFlights: 2, MaxWaiters: 4}})
				if err != nil {
					t.Fatal(err)
				}
				defer tr.CloseIdleConnections()
				for i := 0; i < 2; i++ {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
					if policy == "available-local" {
						if w.Code != 200 || w.Body.String() != "ok" {
							t.Fatal(w.Code, w.Body.String())
						}
						if i == 0 && w.Header().Get("X-Gateway-Degraded") == "" {
							t.Fatal("fallback reason absent")
						}
					} else if w.Code != 503 {
						t.Fatal(w.Code)
					}
				}
				c := m.Snapshot()["counters"].(map[string]int64)
				if policy == "available-local" {
					if calls.Load() != 1 || c["degraded_flights"] != 1 || c["cache_hits"] != 1 {
						t.Fatal(calls.Load(), c)
					}
				} else if calls.Load() != 0 || c["degraded_flights"] != 0 {
					t.Fatal(calls.Load(), c)
				}
				for k, v := range m.Snapshot()["gauges"].(map[string]int64) {
					if v != 0 {
						t.Fatal(k, v)
					}
				}
			})
		}
	}
}

type publicationFault struct {
	sharedcache.Store
	reject bool
}

func (publicationFault) Acquire(context.Context, string, time.Duration) (string, bool, error) {
	return "owner", true, nil
}
func (publicationFault) Release(context.Context, string, string) (bool, error) { return true, nil }
func (s publicationFault) Publish(context.Context, string, string, cache.Response) (bool, error) {
	if s.reject {
		return false, nil
	}
	return false, errors.New("ambiguous publication")
}
func TestPublicationDegradationNeverWarmsOrRefetches(t *testing.T) {
	for _, policy := range []string{"protection-first", "available-local"} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprint(policy, reject), func(t *testing.T) {
				var calls atomic.Int64
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Cache-Control", "public, max-age=10")
					io.WriteString(w, "safe")
				}))
				defer backend.Close()
				local := cache.New(10, 1<<20, nil)
				m := observe.New("test", nil, 0)
				o := leaseSettings()
				o.Degradation = policy
				h, tr, err := NewWithOptions(backend.URL, time.Second, local, Options{Shared: publicationFault{emptyStore{}, reject}, Lease: o, Coalesce: true, Metrics: m})
				if err != nil {
					t.Fatal(err)
				}
				defer tr.CloseIdleConnections()
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
				want := 503
				if policy == "available-local" && !reject {
					want = 200
				}
				if w.Code != want {
					t.Fatal(w.Code, want)
				}
				if n, _ := local.Stats(); n != 0 {
					t.Fatal("ambiguous/rejected result warmed L1")
				}
				if calls.Load() != 1 {
					t.Fatal("publication error retried backend", calls.Load())
				}
				if want == 200 && w.Header().Get("X-Gateway-Degraded") != "publication_error" {
					t.Fatal(w.Header())
				}
			})
		}
	}
}
func TestCacheAdmissionBoundsAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	local := cache.New(10, 1<<20, nil)
	local.Put(backend.URL+`/api/public/hit|""|""|""`, cache.Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}}, Body: []byte("hit"), ExpiresAt: time.Now().Add(time.Second)})
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions(backend.URL, 2*time.Second, local, Options{Coalesce: true, Metrics: m, CacheAdmission: &CacheAdmission{MaxFlights: 1, MaxWaiters: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway"+path, nil))
		return w
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://gateway/api/public/a", nil).WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no active flight")
	}
	if w := request("/api/public/b"); w.Code != 503 || w.Header().Get("X-Gateway-Rejection") != "cache_flights_full" {
		t.Fatal(w.Code, w.Header())
	}
	followerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { followerDone <- request("/api/public/a") }()
	waitCondition(t, func() bool { return m.Snapshot()["gauges"].(map[string]int64)["cache_waiters"] == 2 })
	if w := request("/api/public/c"); w.Code != 503 || w.Header().Get("X-Gateway-Rejection") != "cache_waiters_full" {
		t.Fatal(w.Code, w.Header())
	}
	if w := request("/api/public/hit"); w.Code != 200 || w.Body.String() != "hit" {
		t.Fatal("L1 blocked by cache admission", w.Code)
	}
	cancel()
	<-done
	if w := request("/api/public/b"); w.Code != 503 || w.Header().Get("X-Gateway-Rejection") != "cache_flights_full" {
		t.Fatal("cancellation lost active-flight bound", w.Code, w.Header())
	}
	once.Do(func() { close(release) })
	if w := <-followerDone; w.Code != 200 {
		t.Fatal(w.Code)
	}
	waitCondition(t, func() bool {
		for _, v := range m.Snapshot()["gauges"].(map[string]int64) {
			if v != 0 {
				return false
			}
		}
		return true
	})
	if w := request("/api/public/b"); w.Code != 200 {
		t.Fatal("capacity not released", w.Code)
	}
}

type committedReplyLost struct{ sharedcache.Coordinator }

func (s committedReplyLost) Publish(ctx context.Context, key, token string, v cache.Response) (bool, error) {
	ok, err := s.Coordinator.Publish(ctx, key, token, v)
	if err != nil || !ok {
		return ok, err
	}
	return false, errors.New("reply lost after commit")
}
func TestCommittedPublicationReplyLost(t *testing.T) {
	shared := realCoordinator(t)
	for _, policy := range []string{"protection-first", "available-local"} {
		t.Run(policy, func(t *testing.T) {
			var calls atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "public, max-age=10")
				io.WriteString(w, "committed")
			}))
			defer backend.Close()
			local := cache.New(10, 1<<20, nil)
			m := observe.New("test", nil, 0)
			o := leaseSettings()
			o.Degradation = policy
			h, tr, err := NewWithOptions(backend.URL, time.Second, local, Options{Shared: committedReplyLost{shared}, Lease: o, Coalesce: true, Metrics: m})
			if err != nil {
				t.Fatal(err)
			}
			defer tr.CloseIdleConnections()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
			want := 503
			if policy == "available-local" {
				want = 200
			}
			if w.Code != want {
				t.Fatal(w.Code, want)
			}
			if n, _ := local.Stats(); n != 0 {
				t.Fatal("ambiguous commit admitted locally")
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
			if w.Code != 200 || w.Body.String() != "committed" || calls.Load() != 1 {
				t.Fatal(w.Code, w.Body.String(), calls.Load())
			}
		})
	}
}
func TestAvailableLocalBackendTimeoutDoesNotRefetch(t *testing.T) {
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
	m := observe.New("test", nil, 0)
	o := leaseSettings()
	o.Degradation = "available-local"
	o.FillTimeout = 30 * time.Millisecond
	h, tr, err := NewWithOptions(backend.URL, time.Second, nil, Options{Shared: publicationFault{emptyStore{}, false}, Lease: o, Coalesce: true, Metrics: m, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 504 || calls.Load() != 1 {
		t.Fatal("backend timeout retried", w.Code, calls.Load())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/api/public/item", nil))
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatal("permit lost", w.Code, calls.Load())
	}
}
