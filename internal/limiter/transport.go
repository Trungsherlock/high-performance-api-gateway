package limiter

import (
	"errors"
	"fmt"
	"high-performance-api-gateway/internal/observe"
	"io"
	"net/http"
	"sync"
	"time"
)

var ErrOverloaded = errors.New("upstream concurrency limit reached")
var ErrQueueFull = fmt.Errorf("%w: queue full", ErrOverloaded)
var ErrWaitExpired = fmt.Errorf("%w: wait timeout", ErrOverloaded)

type Transport struct {
	Metrics *observe.Metrics
	Next    http.RoundTripper
	permits chan struct{}
	wait    time.Duration
	waiters chan struct{}
}

// New preserves the original policy: bounded duration, unbounded waiter count.
func New(next http.RoundTripper, capacity int, wait time.Duration) *Transport {
	return NewWithQueue(next, capacity, wait, -1)
}

// NewWithQueue bounds queued permit waiters; -1 selects the legacy policy.
// Admission is not FIFO. Permits cover the complete upstream body lifetime.
func NewWithQueue(next http.RoundTripper, capacity int, wait time.Duration, maxWaiters int) *Transport {
	if capacity <= 0 || wait < 0 || maxWaiters < -1 {
		panic("invalid limiter configuration")
	}
	t := &Transport{Next: next, permits: make(chan struct{}, capacity), wait: wait}
	if maxWaiters >= 0 {
		t.waiters = make(chan struct{}, maxWaiters)
	}
	return t
}
func (t *Transport) reject(reason error) error {
	t.Metrics.Add("permit_rejections", 1)
	if errors.Is(reason, ErrQueueFull) {
		t.Metrics.Add("permit_queue_full", 1)
	} else {
		t.Metrics.Add("permit_wait_timeout", 1)
	}
	return reason
}
func (t *Transport) acquire(r *http.Request) error {
	started := time.Now()
	defer func() { t.Metrics.Observe("permit_wait", time.Since(started)) }()
	if err := r.Context().Err(); err != nil {
		return err
	}
	select {
	case t.permits <- struct{}{}:
		return nil
	default:
	}
	if t.waiters != nil {
		select {
		case t.waiters <- struct{}{}:
			defer func() { <-t.waiters }()
		default:
			return t.reject(ErrQueueFull)
		}
	}
	t.Metrics.Gauge("permit_waiters", 1)
	defer t.Metrics.Gauge("permit_waiters", -1)
	if t.wait == 0 {
		return t.reject(ErrWaitExpired)
	}
	timer := time.NewTimer(t.wait)
	defer timer.Stop()
	select {
	case t.permits <- struct{}{}:
		return nil
	case <-r.Context().Done():
		return r.Context().Err()
	case <-timer.C:
		return t.reject(ErrWaitExpired)
	}
}
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.acquire(r); err != nil {
		return nil, err
	}
	if err := r.Context().Err(); err != nil {
		<-t.permits
		return nil, err
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
