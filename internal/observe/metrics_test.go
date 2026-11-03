package observe

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) { b.Lock(); defer b.Unlock(); return b.b.Write(p) }
func (b *lockedBuffer) String() string              { b.Lock(); defer b.Unlock(); return b.b.String() }
func TestLogsExcludeRequestDataAndCountAbort(t *testing.T) {
	out := &lockedBuffer{}
	m := New("test", out, 1)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("abort not preserved")
			}
		}()
		req := httptest.NewRequest("GET", "/secret?token=password", nil)
		req.Header.Set("Authorization", "secret")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	deadline := time.Now().Add(time.Second)
	for out.String() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	v := out.String()
	if v == "" || strings.Contains(v, "password") || strings.Contains(v, "secret") {
		t.Fatalf("unsafe/missing log: %s", v)
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	if c["aborted"] != 1 || c["completed"] != 1 {
		t.Fatal(c)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestUpstreamGaugeCoversBody(t *testing.T) {
	m := New("test", nil, 0)
	tr := &Transport{Metrics: m, Next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{Body: io.NopCloser(strings.NewReader("payload"))}, nil
	})}
	resp, err := tr.RoundTrip(httptest.NewRequest("GET", "http://backend/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if m.Snapshot()["gauges"].(map[string]int64)["active_backend"] != 1 {
		t.Fatal("released at headers")
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if m.Snapshot()["gauges"].(map[string]int64)["active_backend"] != 0 {
		t.Fatal("leaked gauge")
	}
	h := m.Snapshot()["histograms"].(map[string]Histogram)
	if h["upstream"].Count != 1 {
		t.Fatal("double observed")
	}
}

// A blocked log sink must never become an unbounded request queue.
type blockedSink struct{ release chan struct{} }

func (s blockedSink) Write(p []byte) (int, error) { <-s.release; return len(p), nil }
func TestBlockedLogSinkDropsWithoutBlockingHandlers(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	m := New("test", blockedSink{release}, 1)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < 600; i++ {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("blocked log sink blocked handlers")
	}
	c := m.Snapshot()["counters"].(map[string]int64)
	if c["completed"] != 600 || c["logs_dropped"] == 0 {
		t.Fatal(c)
	}
}
