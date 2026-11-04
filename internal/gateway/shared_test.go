package gateway

import (
	"context"
	"errors"
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

func TestSharedRedisModes(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR to an isolated Redis")
	}
	for _, mode := range []string{"local", "shared", "layered"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "public, max-age=1")
				io.WriteString(w, "public")
			}))
			defer backend.Close()
			shared, err := sharedcache.New(addr, 200*time.Millisecond, 4)
			if err != nil {
				t.Fatal(err)
			}
			defer shared.Close()
			type node struct {
				server  *httptest.Server
				metrics *observe.Metrics
			}
			makeNode := func() node {
				var local *cache.Cache
				if mode != "shared" {
					local = cache.New(10, 1<<20, nil)
				}
				var store sharedcache.Store
				if mode != "local" {
					store = shared
				}
				m := observe.New("test", nil, 0)
				h, tr, err := NewWithOptions(backend.URL, time.Second, local, Options{Shared: store, Coalesce: true, Metrics: m})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(tr.CloseIdleConnections)
				s := httptest.NewServer(h)
				t.Cleanup(s.Close)
				return node{s, m}
			}
			a, b := makeNode(), makeNode()
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 2 * time.Second}
			defer client.CloseIdleConnections()
			request := func(n node, header string) {
				req, _ := http.NewRequest("GET", n.server.URL+"/api/public/item?id=42", nil)
				if header != "" {
					req.Header.Set(header, "x")
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || string(body) != "public" {
					t.Fatal(resp.StatusCode, string(body))
				}
			}
			request(a, "")
			request(b, "")
			want := int64(1)
			if mode == "local" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatal("fills", calls.Load(), want)
			}
			for _, header := range []string{"Authorization", "Cookie", "Accept-Language"} {
				request(a, header)
				request(b, header)
				want += 2
			}
			if calls.Load() != want {
				t.Fatal("unsafe request cached", calls.Load(), want)
			}
			// Empty L1 promotions repeatedly approach the original shared expiry.
			if mode != "local" {
				time.Sleep(400 * time.Millisecond)
				c := makeNode()
				request(c, "")
				if calls.Load() != want {
					t.Fatal("promotion missed")
				}
				time.Sleep(700 * time.Millisecond)
				request(c, "")
				want++
				if calls.Load() != want {
					t.Fatal("promotion restarted TTL", calls.Load(), want)
				}
			}
		})
	}
}
func TestSharedColdMissesStillDuplicate(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR")
	}
	var calls atomic.Int64
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	shared, _ := sharedcache.New(addr, 200*time.Millisecond, 4)
	defer shared.Close()
	var nodes []*httptest.Server
	for i := 0; i < 2; i++ {
		h, tr, err := NewWithOptions(backend.URL, 2*time.Second, nil, Options{Shared: shared, Coalesce: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		s := httptest.NewServer(h)
		defer s.Close()
		nodes = append(nodes, s)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for _, n := range nodes {
		go func(s *httptest.Server) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), "GET", s.URL+"/api/public/item", nil)
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(n)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			wg.Wait()
			t.Fatal("two fills did not overlap")
		}
	}
	close(release)
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}

type unavailableStore struct{}

func (unavailableStore) Get(context.Context, string) (cache.Response, bool, error) {
	return cache.Response{}, false, errors.New("dependency unavailable")
}
func (unavailableStore) Put(context.Context, string, cache.Response) error {
	return errors.New("dependency unavailable")
}
func TestSharedFailureFallsBackAndLocalHitsSkipDependency(t *testing.T) {
	var calls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	m := observe.New("test", nil, 0)
	h, tr, err := NewWithOptions(backend.URL, time.Second, cache.New(10, 1<<20, nil), Options{Shared: unavailableStore{}, Coalesce: true, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "http://gateway/api/public/item", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	counters := m.Snapshot()["counters"].(map[string]int64)
	if calls.Load() != 1 || counters["dependency_errors"] != 2 || counters["dependency_error_other"] != 2 || !causesReconcile(counters) || counters["shared_read_errors"] != 1 || counters["shared_write_errors"] != 1 || counters["shared_misses"] != 0 || counters["cache_hits"] != 1 {
		t.Fatal(calls.Load(), counters)
	}
}

// causesReconcile reports whether every dependency error has exactly one fixed cause.
func causesReconcile(c map[string]int64) bool {
	return c["dependency_error_timeout"]+c["dependency_error_pool_timeout"]+c["dependency_error_connection"]+c["dependency_error_other"] == c["dependency_errors"]
}
func TestSharedRejectsUnsafeResponses(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR")
	}
	for _, scenario := range []string{"private", "cookie", "variant", "large", "encoding"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "public, max-age=10")
				switch scenario {
				case "private":
					w.Header().Set("Cache-Control", "private, max-age=10")
				case "cookie":
					w.Header().Set("Set-Cookie", "secret=1")
				case "variant":
					w.Header().Set("Vary", "Accept-Language")
				case "encoding":
					w.Header().Set("Content-Encoding", "custom")
				}
				if scenario == "large" {
					io.WriteString(w, strings.Repeat("x", 65537))
				} else {
					io.WriteString(w, "ok")
				}
			}))
			defer backend.Close()
			shared, _ := sharedcache.New(addr, 200*time.Millisecond, 2)
			defer shared.Close()
			m := observe.New("test", nil, 0)
			h, tr, err := NewWithOptions(backend.URL, time.Second, nil, Options{Shared: shared, Metrics: m})
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
			}
			c := m.Snapshot()["counters"].(map[string]int64)
			if calls.Load() != 2 || c["shared_hits"] != 0 || c["shared_write_completed"] != 0 {
				t.Fatal(calls.Load(), c)
			}
		})
	}
}
