package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testProxy(t *testing.T, upstream string, timeout time.Duration) *httptest.Server {
	t.Helper()
	h, transport, err := New(upstream, timeout)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(func() { s.Close(); transport.CloseIdleConnections() })
	return s
}

func TestForwarding(t *testing.T) {
	for _, status := range []int{200, 302, 404, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.EscapedPath() != "/base/a%2Fb" || r.URL.RawQuery != "id=1&id=2" || string(body) != "payload" || r.Header.Get("X-End-To-End") != "kept" {
					t.Errorf("request changed: %s %s body=%s", r.Method, r.URL, body)
				}
				if r.Header.Get("X-Hop") != "" || r.Header.Get("Connection") != "" || r.Header.Get("X-Forwarded-For") != "" {
					t.Error("unsafe headers forwarded")
				}
				w.Header().Set("Connection", "X-Response-Hop")
				w.Header().Set("X-Response-Hop", "remove")
				w.Header().Set("X-Response", "keep")
				w.Header().Set("Location", "/elsewhere")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("response"))
			}))
			defer upstream.Close()
			proxy := testProxy(t, upstream.URL+"/base", time.Second)
			req, _ := http.NewRequest("POST", proxy.URL+"/a%2Fb?id=1&id=2", strings.NewReader("payload"))
			req.Header.Set("Connection", "X-Hop")
			req.Header.Set("X-Hop", "remove")
			req.Header.Set("X-End-To-End", "kept")
			req.Header.Set("X-Forwarded-For", "spoofed")
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != status || string(body) != "response" || resp.Header.Get("X-Response") != "keep" || resp.Header.Get("Location") != "/elsewhere" || resp.Header.Get("X-Response-Hop") != "" || resp.Header.Get("Connection") != "" {
				t.Fatalf("response changed: %d %s %v", resp.StatusCode, body, resp.Header)
			}
		})
	}
}

func TestGETAndFixedTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.RequestURI() != "/item?q=42" {
			t.Errorf("request changed: %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	h, transport, err := New(upstream.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://untrusted.invalid/item?q=42", nil))
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
}

func TestUnavailable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	proxy := testProxy(t, "http://"+addr, time.Second)
	resp, err := http.Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancellation"}[cancelClient], func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(stopped)
			}))
			defer upstream.Close()
			timeout := 100 * time.Millisecond
			if cancelClient {
				timeout = 5 * time.Second
			}
			proxy := testProxy(t, upstream.URL, timeout)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "GET", proxy.URL, nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					if !cancelClient {
						t.Error(err)
					}
					return
				}
				defer resp.Body.Close()
				if !cancelClient && resp.StatusCode != 504 {
					t.Errorf("status=%d", resp.StatusCode)
				}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never started")
			}
			if cancelClient {
				cancel()
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream not canceled")
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("client stuck")
			}
		})
	}
}

func TestUnsupportedAndInvalid(t *testing.T) {
	for _, upstream := range []string{"", "/relative", "ftp://host", "http://user:pass@host", "http://host?q=x", "http://host/#fragment"} {
		if _, _, err := New(upstream, time.Second); err == nil {
			t.Errorf("accepted %q", upstream)
		}
	}
	if _, _, err := New("http://localhost", 0); err == nil {
		t.Error("accepted zero timeout")
	}
	h, transport, _ := New("http://localhost:1", time.Second)
	defer transport.CloseIdleConnections()
	for _, method := range []string{"CONNECT", "GET"} {
		req := httptest.NewRequest(method, "http://localhost/", nil)
		if method == "GET" {
			req.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 501 {
			t.Errorf("status=%d", w.Code)
		}
	}
}
