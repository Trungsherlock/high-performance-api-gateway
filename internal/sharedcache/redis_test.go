package sharedcache

import (
	"context"
	"encoding/json"
	"high-performance-api-gateway/internal/cache"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func value() cache.Response {
	return cache.Response{Header: http.Header{"Cache-Control": {"public, max-age=10"}, "Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`), ExpiresAt: time.Now().Add(time.Second)}
}
func TestDecode(t *testing.T) {
	v := value()
	raw, _ := json.Marshal(envelope{Version: 1, ExpiresUnixMilli: v.ExpiresAt.UnixMilli(), Header: v.Header, Body: v.Body})
	start := time.Now().Add(-100 * time.Millisecond)
	got, ok, err := decode(raw, 500, start)
	if err != nil || !ok || got.ExpiresAt.After(start.Add(500*time.Millisecond)) {
		t.Fatal(got, ok, err)
	}
	for name, raw := range map[string][]byte{"corrupt": []byte("{"), "schema": []byte(`{"version":2}`), "oversize": []byte(strings.Repeat("x", MaxValue+1)), "private": []byte(`{"version":1,"header":{"Cache-Control":["private, max-age=10"]}}`), "unsafe": []byte(`{"version":1,"header":{"Cache-Control":["public, max-age=10"],"Set-Cookie":["secret"]}}`)} {
		t.Run(name, func(t *testing.T) {
			if _, ok, err := decode(raw, 1000, time.Now()); ok || err != nil {
				t.Fatal(ok, err)
			}
		})
	}
	if _, ok, _ := decode(raw, 50, start); ok {
		t.Fatal("lookup restarted TTL")
	}
	if _, ok, _ := decode(raw, -1, time.Now()); ok {
		t.Fatal("persistent/expired admitted")
	}
}
func TestUnavailableAndCancellation(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	r, _ := New(addr, 30*time.Millisecond, 1)
	defer r.Close()
	start := time.Now()
	_, ok, err := r.Get(context.Background(), "x")
	if err == nil || ok || time.Since(start) > time.Second {
		t.Fatal(ok, err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Get(ctx, "x"); err == nil {
		t.Fatal("cancellation lost")
	}
}
func TestRedisIntegration(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR to an isolated Redis")
	}
	r, err := New(addr, 200*time.Millisecond, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	key := "integration-" + t.Name() + time.Now().String()
	v := value()
	if err := r.Put(ctx, key, v); err != nil {
		t.Fatal(err)
	}
	defer r.client.Del(ctx, Key(key))
	got, ok, err := r.Get(ctx, key)
	if err != nil || !ok || string(got.Body) != string(v.Body) || got.ExpiresAt.After(v.ExpiresAt) {
		t.Fatal(got, ok, err)
	}
	for i := 0; i < 3; i++ {
		got, ok, err = r.Get(ctx, key)
		if err != nil || !ok || got.ExpiresAt.After(v.ExpiresAt) {
			t.Fatal("TTL extended", got, ok, err)
		}
	}
	cases := map[string]string{"corrupt": "{", "schema": `{"version":99}`, "oversized": strings.Repeat("x", MaxValue+1)}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			r.client.Set(ctx, Key(key), raw, time.Second)
			if _, ok, err := r.Get(ctx, key); ok || err != nil {
				t.Fatal(ok, err)
			}
		})
	}
	r.client.Set(ctx, Key(key), "{}", 0)
	if _, ok, err := r.Get(ctx, key); ok || err != nil {
		t.Fatal("persistent key", ok, err)
	}
	// A wrong Redis data type is an operation error, not a genuine miss.
	r.client.Del(ctx, Key(key))
	r.client.LPush(ctx, Key(key), "x")
	if _, _, err := r.Get(ctx, key); err == nil {
		t.Fatal("wrong type not reported")
	}
}

func TestSlowDependencyAndPoolBound(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	defer func() {
		l.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	}()
	r, _ := New(l.Addr().String(), 40*time.Millisecond, 2)
	defer r.Close()
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := r.Get(context.Background(), "x"); err == nil {
				t.Error("silent server must fail")
			}
		}()
	}
	wg.Wait()
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("unbounded dependency wait", time.Since(start))
	}
	if r.client.PoolStats().TotalConns > 2 {
		t.Fatal("connection cap exceeded")
	}
}
