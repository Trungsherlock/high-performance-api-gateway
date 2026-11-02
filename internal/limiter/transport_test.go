package limiter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPermitLivesThroughBody(t *testing.T) {
	next := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{Body: io.NopCloser(strings.NewReader("body"))}, nil
	})
	limiter := New(next, 1, time.Millisecond)
	request := httptest.NewRequest("GET", "http://backend/", nil)
	first, err := limiter.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limiter.RoundTrip(request); !errors.Is(err, ErrOverloaded) {
		t.Fatal(err)
	}
	io.ReadAll(first.Body)
	second, err := limiter.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close() // Must not release the second request's permit.
	if _, err := limiter.RoundTrip(request); !errors.Is(err, ErrOverloaded) {
		t.Fatal(err)
	}
	second.Body.Close()
	third, err := limiter.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	third.Body.Close()
}
func TestErrorAndCancellationRelease(t *testing.T) {
	want := errors.New("upstream failure")
	limiter := New(transportFunc(func(*http.Request) (*http.Response, error) { return nil, want }), 1, time.Millisecond)
	r := httptest.NewRequest("GET", "http://backend/", nil)
	for range 2 {
		if _, err := limiter.RoundTrip(r); !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.RoundTrip(r.WithContext(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(limiter.permits) != 0 {
		t.Fatal("leaked permit")
	}
}
