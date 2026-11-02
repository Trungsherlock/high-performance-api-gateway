package limiter

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

var ErrOverloaded = errors.New("upstream concurrency limit reached")

type Transport struct {
	Next    http.RoundTripper
	permits chan struct{}
	wait    time.Duration
}

func New(next http.RoundTripper, capacity int, wait time.Duration) *Transport {
	if capacity <= 0 || wait < 0 {
		panic("invalid limiter configuration")
	}
	return &Transport{Next: next, permits: make(chan struct{}, capacity), wait: wait}
}
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	default:
	}
	select {
	case t.permits <- struct{}{}:
	default:
		timer := time.NewTimer(t.wait)
		defer timer.Stop()
		select {
		case t.permits <- struct{}{}:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-timer.C:
			return nil, ErrOverloaded
		}
	}
	release := func() { <-t.permits }
	response, err := t.Next.RoundTrip(r)
	if err != nil {
		release()
		return response, err
	}
	if response.Body == nil {
		release()
		return response, nil
	}
	response.Body = &body{ReadCloser: response.Body, release: release}
	return response, nil
}

type body struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}
func (b *body) Close() error { err := b.ReadCloser.Close(); b.once.Do(b.release); return err }
