package mockbackend

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// Backend counts data requests, excluding health and statistics requests.
type Backend struct {
	delay time.Duration
	calls atomic.Uint64
}

func New(delay time.Duration) *Backend { return &Backend{delay: delay} }

func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/healthz":
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
		return
	case "/stats":
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Calls uint64 `json:"calls"`
		}{b.calls.Load()})
		return
	}
	b.calls.Add(1)
	if r.Method != http.MethodGet || r.URL.Path != "/api/public/item" {
		http.NotFound(w, r)
		return
	}
	if b.delay > 0 {
		timer := time.NewTimer(b.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=10")
	_ = json.NewEncoder(w).Encode(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{r.URL.Query().Get("id"), "demo item"})
}
