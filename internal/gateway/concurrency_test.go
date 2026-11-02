package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/coalesce"
	"high-performance-api-gateway/internal/limiter"
)

func concurrentProxy(t *testing.T, upstream string, storage *cache.Cache, limit int, timeout time.Duration) (*httptest.Server, *http.Client) {
	t.Helper()
	lifecycle, cancel := context.WithCancel(context.Background())
	h, tr, err := NewWithOptions(upstream, timeout, storage, Options{Coalesce: true, MaxConcurrent: limit, PermitWait: 10 * time.Millisecond, Lifecycle: lifecycle})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 3 * time.Second}
	t.Cleanup(func() { cancel(); s.Close(); tr.CloseIdleConnections(); client.CloseIdleConnections() })
	return s, client
}
func getStatus(client *http.Client, url string) (int, string, error) {
	r, err := client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	return r.StatusCode, string(b), err
}
func TestColdAndExpiredBursts(t *testing.T) {
	var count atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "shared")
	}))
	defer upstream.Close()
	var expired atomic.Bool
	storage := cache.New(1000, 16<<20, func() time.Time {
		if expired.Load() {
			return time.Now().Add(time.Minute)
		}
		return time.Now()
	})
	proxy, client := concurrentProxy(t, upstream.URL, storage, 32, time.Second)
	for round := 0; round < 2; round++ {
		if round == 1 {
			expired.Store(true)
			storage.Get(upstream.URL + "/api/public/item|\"\"|\"\"|\"Go-http-client/1.1\"")
			expired.Store(false)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 100 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				status, body, err := getStatus(client, proxy.URL+"/api/public/item")
				if err != nil || status != 200 || body != "shared" {
					t.Errorf("%d %q %v", status, body, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if count.Load() != int32(round+1) {
			t.Fatalf("round %d calls=%d", round, count.Load())
		}
	}
}
func TestLeaderDisconnectAndFollowerCancellation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	proxy, client := concurrentProxy(t, upstream.URL, cache.New(1000, 16<<20, nil), 1, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", proxy.URL+"/api/public/item", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, _ := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no leader")
	}
	cancel()
	<-done
	// Cancel a follower while the leader's shared operation remains active.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, "GET", proxy.URL+"/api/public/item", nil)
	client.Do(req2)
	close(release)
	status, body, err := getStatus(client, proxy.URL+"/api/public/item")
	if status != 200 || body != "ok" || err != nil || calls.Load() != 1 {
		t.Fatalf("%d %s %v calls=%d", status, body, err, calls.Load())
	}
}
func TestLimiterBypassAndCacheHit(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var active, peak atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/public/hot" {
			w.Header().Set("Cache-Control", "public, max-age=10")
			io.WriteString(w, "hot")
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	proxy, client := concurrentProxy(t, upstream.URL, cache.New(1000, 16<<20, nil), 2, 10*time.Second)
	if status, _, err := getStatus(client, proxy.URL+"/api/public/hot"); status != 200 || err != nil {
		t.Fatal(status, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); getStatus(client, fmt.Sprintf("%s/private/%d", proxy.URL, i)) }(i)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("no active requests")
		}
	}
	var burst sync.WaitGroup
	for i := 0; i < 100; i++ {
		burst.Add(1)
		go func(i int) {
			defer burst.Done()
			status, _, err := getStatus(client, fmt.Sprintf("%s/api/public/distinct?id=%d", proxy.URL, i))
			if status != 503 || err != nil {
				t.Errorf("overload: %d %v", status, err)
			}
		}(i)
	}
	burst.Wait()
	status, body, err := getStatus(client, proxy.URL+"/api/public/hot")
	close(release)
	wg.Wait()
	if status != 200 || body != "hot" || err != nil {
		t.Fatal(status, body, err)
	}
	if peak.Load() > 2 {
		t.Fatal("limit exceeded")
	}
}

func TestSharedTimeoutRecovery(t *testing.T) {
	var calls atomic.Int32
	stopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			close(stopped)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=10")
		io.WriteString(w, "recovered")
	}))
	defer upstream.Close()
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	ct := &cacheTransport{next: limiter.New(transport, 1, 10*time.Millisecond), cache: cache.New(1000, 16<<20, nil), group: &coalesce.Group[sharedResult]{}, timeout: 100 * time.Millisecond, lifecycle: context.Background()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", upstream.URL+"/api/public/item", nil)
	_, err := ct.RoundTrip(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shared fetch was not canceled")
	}
	// RoundTrip waited for flight completion; retry must acquire the released permit.
	response, err := ct.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "recovered" || err != nil {
		t.Fatal(response.StatusCode, string(body), err)
	}
}

func TestUnshareableFallbackAndPrivateBypass(t *testing.T) {
	for _, kind := range []string{"private", "large", "error", "cookie"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			body := "private body"
			status := 200
			if kind == "large" {
				body = strings.Repeat("x", 70<<10)
			}
			if kind == "error" {
				status = 500
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "public, max-age=10")
				if kind == "private" {
					w.Header().Set("Set-Cookie", "session=secret")
				}
				w.WriteHeader(status)
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			proxy, client := concurrentProxy(t, upstream.URL, cache.New(1000, 16<<20, nil), 1, time.Second)
			for range 2 {
				req, _ := http.NewRequest("GET", proxy.URL+"/api/public/item", nil)
				if kind == "cookie" {
					req.Header.Set("Cookie", "session=secret")
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				received, err := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != status || string(received) != body || err != nil {
					t.Fatal("fallback response changed")
				}
			}
			want := int32(4)
			if kind == "cookie" {
				want = 2
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want %d", calls.Load(), want)
			}
		})
	}
}
