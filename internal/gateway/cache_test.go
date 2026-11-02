package gateway

import (
	"high-performance-api-gateway/internal/cache"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheIntegration(t *testing.T) {
	for _, scenario := range []string{"hit", "query", "expired", "cookie", "auth", "vary", "private", "no-store", "set-cookie", "large", "chunked-large", "partial", "error", "post", "private-path", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			body := "hello"
			if scenario == "large" || scenario == "chunked-large" {
				body = strings.Repeat("x", 70<<10)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "public, max-age=10")
				switch scenario {
				case "vary":
					w.Header().Set("Vary", "Accept")
				case "private":
					w.Header().Set("Cache-Control", "private, max-age=10")
				case "no-store":
					w.Header().Set("Cache-Control", "public, max-age=10, no-store")
				case "set-cookie":
					w.Header().Set("Set-Cookie", "session=secret")
				case "partial":
					w.Header().Set("Content-Length", "100")
				case "error":
					w.WriteHeader(500)
				case "chunked-large":
					w.(http.Flusher).Flush()
				}
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			storage := cache.New(1000, 16<<20, func() time.Time { return time.Unix(0, clock.Load()) })
			if scenario == "disabled" {
				storage = nil
			}
			handler, transport, err := NewWithCache(upstream.URL, time.Second, storage)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			proxy := httptest.NewServer(handler)
			defer proxy.Close()
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			defer client.CloseIdleConnections()
			for i := 0; i < 2; i++ {
				path := "/api/public/item?id=1"
				method := "GET"
				if scenario == "query" && i == 1 {
					path = "/api/public/item?id=2"
				}
				if scenario == "expired" && i == 1 {
					clock.Add(int64(time.Minute))
				}
				if scenario == "private-path" {
					path = "/api/private/item"
				}
				if scenario == "post" {
					method = "POST"
				}
				req, _ := http.NewRequest(method, proxy.URL+path, nil)
				if scenario == "cookie" {
					req.Header.Set("Cookie", "session=abc")
				}
				if scenario == "auth" {
					req.Header.Set("Authorization", "Bearer abc")
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				received, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				wantStatus := 200
				if scenario == "error" {
					wantStatus = 500
				}
				if scenario == "partial" {
					wantStatus = 502
				}
				if resp.StatusCode != wantStatus {
					t.Fatalf("status=%d want %d", resp.StatusCode, wantStatus)
				}
				if scenario != "partial" && (err != nil || string(received) != body) {
					t.Fatalf("body mismatch or error: %v", err)
				}
			}
			want := int32(2)
			if scenario == "hit" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want %d", calls.Load(), want)
			}
		})
	}
}
