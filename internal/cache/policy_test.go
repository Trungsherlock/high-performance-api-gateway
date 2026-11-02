package cache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestPolicy(t *testing.T) {
	for _, name := range []string{"Authorization", "Cookie", "Range", "If-None-Match", "If-Modified-Since", "Cache-Control", "Accept-Language", "X-Tenant", "Connection"} {
		r := httptest.NewRequest("GET", "/api/public/item", nil)
		r.Header.Set(name, "value")
		if Eligible(r) {
			t.Errorf("accepted %s", name)
		}
	}
	if !Eligible(httptest.NewRequest("GET", "/api/public/item", nil)) {
		t.Fatal("public GET rejected")
	}
	for _, path := range []string{"/api/private/item", "/api/publicity/item"} {
		if Eligible(httptest.NewRequest("GET", path, nil)) {
			t.Fatal(path)
		}
	}
	if Eligible(httptest.NewRequest("POST", "/api/public/item", nil)) {
		t.Fatal("POST accepted")
	}
}
func TestResponsePolicy(t *testing.T) {
	for _, control := range []string{"", "public", "public, max-age=0", "public, max-age=10, private", "public, max-age=10, no-store", "public, max-age=10, no-cache", "public, max-age=10, max-age=20", "public, max-age=abc"} {
		r := &http.Response{StatusCode: 200, Header: http.Header{"Cache-Control": {control}}}
		if Lifetime(r, 10*time.Second) != 0 {
			t.Errorf("accepted %s", control)
		}
	}
	r := &http.Response{StatusCode: 200, Header: http.Header{"Cache-Control": {"public, max-age=20"}}}
	if Lifetime(r, 10*time.Second) != 10*time.Second {
		t.Fatal("TTL cap failed")
	}
	r.Header.Set("Age", "19")
	if Lifetime(r, 10*time.Second) != time.Second {
		t.Fatal("Age ignored")
	}
	for _, name := range []string{"Set-Cookie", "Vary", "Content-Encoding", "Content-Range"} {
		r.Header.Set(name, "")
		if Lifetime(r, 10*time.Second) != 0 {
			t.Fatal(name)
		}
		r.Header.Del(name)
	}
	r.StatusCode = 500
	if Lifetime(r, 10*time.Second) != 0 {
		t.Fatal("cached 500")
	}
}
