package gateway

import (
	"context"
	"errors"
	"fmt"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/sharedcache"
	"math/rand/v2"
	"net/http"
	"time"
)

var ErrCoordination = errors.New("shared coordination failed")
var ErrPublication = fmt.Errorf("%w: publication uncertain", ErrCoordination)
var ErrLeaseLost = fmt.Errorf("%w: ownership lost", ErrCoordination)
var ErrLeaseWait = fmt.Errorf("%w: wait expired", ErrCoordination)

type LeaseOptions struct {
	Degradation  string // empty defaults to protection-first
	Duration     time.Duration
	FillTimeout  time.Duration
	WaitTimeout  time.Duration
	PollInterval time.Duration
	Margin       time.Duration
}

func (o LeaseOptions) valid() bool {
	return (o.Degradation == "" || o.Degradation == "protection-first" || o.Degradation == "available-local") && o.FillTimeout > 0 && o.Margin > 0 && o.Duration > o.Margin && o.FillTimeout < o.Duration-o.Margin && o.WaitTimeout > 0 && o.PollInterval >= time.Millisecond && o.PollInterval <= time.Second && o.Duration <= time.Minute && o.WaitTimeout <= time.Minute
}

func (t *cacheTransport) leaseError(err error) error {
	t.metrics.Add("dependency_errors", 1)
	t.metrics.Add("lease_errors", 1)
	return fmt.Errorf("%w: %v", ErrCoordination, err)
}
func (t *cacheTransport) leaseRead(ctx context.Context, key string) (cache.Response, bool, error) {
	start := time.Now()
	v, ok, err := t.shared.Get(ctx, key)
	t.metrics.Observe("cache_dependency", time.Since(start))
	if err != nil {
		t.metrics.Add("shared_read_errors", 1)
		return cache.Response{}, false, t.leaseError(err)
	}
	if !ok || !time.Now().Before(v.ExpiresAt) {
		t.metrics.Add("shared_misses", 1)
		return cache.Response{}, false, nil
	}
	t.metrics.Add("shared_hits", 1)
	if t.cache != nil {
		t.cache.Put(key, v)
	}
	return v, true, nil
}
func (t *cacheTransport) coordinated(ctx context.Context, r *http.Request, key string) (sharedResult, error) {
	start := time.Now()
	wait, cancel := context.WithTimeout(ctx, t.lease.WaitTimeout)
	defer cancel()
	contended := false
	finished := false
	finishWait := func() {
		if finished {
			return
		}
		finished = true
		t.metrics.Observe("coordination_wait", time.Since(start))
		if contended {
			t.metrics.Gauge("distributed_waiters", -1)
		}
	}
	defer finishWait()
	for {
		if wait.Err() != nil {
			t.metrics.Add("lease_wait_timeouts", 1)
			return sharedResult{}, ErrLeaseWait
		}
		v, ok, err := t.leaseRead(wait, key)
		if err != nil {
			if wait.Err() != nil {
				t.metrics.Add("lease_wait_timeouts", 1)
				return sharedResult{}, ErrLeaseWait
			}
			return sharedResult{}, err
		}
		if ok {
			return sharedResult{value: v, shareable: true}, nil
		}
		acquiredAt := time.Now()
		op := time.Now()
		token, owned, err := t.leases.Acquire(wait, key, t.lease.Duration)
		t.metrics.Observe("cache_dependency", time.Since(op))
		if err != nil {
			dependencyError := t.leaseError(err)
			if wait.Err() != nil {
				t.metrics.Add("lease_wait_timeouts", 1)
				return sharedResult{}, ErrLeaseWait
			}
			return sharedResult{}, dependencyError
		}
		if owned {
			t.metrics.Add("lease_acquired", 1)
			finishWait()
			return t.fillOwned(ctx, r, key, token, acquiredAt)
		}
		if !contended {
			contended = true
			t.metrics.Add("distributed_followers", 1)
			t.metrics.Gauge("distributed_waiters", 1)
		}
		t.metrics.Add("lease_polls", 1)
		// Bounded jitter; unrelated keys wait independently, and cancellation wakes timers.
		delay := t.lease.PollInterval + time.Duration(rand.Int64N(int64(t.lease.PollInterval)))
		timer := time.NewTimer(delay)
		select {
		case <-wait.Done():
			timer.Stop()
			t.metrics.Add("lease_wait_timeouts", 1)
			return sharedResult{}, ErrLeaseWait
		case <-timer.C:
		}
	}
}
func (t *cacheTransport) fillOwned(ctx context.Context, r *http.Request, key, token string, acquiredAt time.Time) (sharedResult, error) {
	defer func() {
		start := time.Now()
		released, err := t.leases.Release(context.WithoutCancel(ctx), key, token)
		t.metrics.Observe("cache_dependency", time.Since(start))
		if err != nil {
			t.leaseError(err)
		} else if released {
			t.metrics.Add("lease_releases", 1)
		}
	}()
	deadline := time.Now().Add(t.lease.FillTimeout)
	latest := acquiredAt.Add(t.lease.Duration - t.lease.Margin)
	if deadline.After(latest) {
		deadline = latest
	}
	fill, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// Recheck after acquiring: another owner may have published just before SET NX.
	v, ok, err := t.leaseRead(fill, key)
	if err != nil {
		return sharedResult{}, err
	}
	if ok {
		return sharedResult{value: v, shareable: true}, nil
	}
	if err := fill.Err(); err != nil {
		return sharedResult{}, err
	}
	response, value, err := t.fetch(r.Clone(fill), key, false)
	if err != nil {
		return sharedResult{}, err
	}
	defer response.Body.Close()
	if value == nil {
		return sharedResult{}, nil
	} // Existing per-caller streaming fallback.
	op := time.Now()
	published, err := t.leases.Publish(fill, key, token, *value)
	t.metrics.Observe("cache_dependency", time.Since(op))
	if errors.Is(err, sharedcache.ErrUnshareable) {
		return sharedResult{}, nil
	}
	if err != nil {
		t.metrics.Add("shared_write_errors", 1)
		dependencyError := t.leaseError(err)
		if t.lease.Degradation == "available-local" && fill.Err() == nil && time.Now().Before(value.ExpiresAt) {
			t.metrics.Fallback("publication_error")
			// A valid completed response may be delivered to this local flight, but is
			// never admitted to L1 after an ambiguous publication. No second fetch.
			return sharedResult{value: *value, shareable: true, degraded: "publication_error"}, nil
		}
		return sharedResult{}, fmt.Errorf("%w: %v", ErrPublication, dependencyError)
	}
	if !published {
		t.metrics.Add("lease_publish_rejected", 1)
		return sharedResult{}, ErrLeaseLost
	}
	t.metrics.Add("lease_publications", 1)
	t.metrics.Add("shared_write_completed", 1)
	if !time.Now().Before(value.ExpiresAt) {
		return sharedResult{}, ErrLeaseLost
	}
	if t.cache != nil {
		t.cache.Put(key, *value)
	}
	return sharedResult{value: *value, shareable: true}, nil
}

// Available-local fallback only handles failed coordination before a fill. Lost
// ownership is always rejected, and publication errors never trigger another fetch.
func (t *cacheTransport) withDegradation(ctx context.Context, r *http.Request, key string) (sharedResult, error) {
	result, err := t.coordinated(ctx, r, key)
	if err == nil || t.lease.Degradation != "available-local" || errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrPublication) || !errors.Is(err, ErrCoordination) || ctx.Err() != nil {
		return result, err
	}
	reason := "dependency_error"
	if errors.Is(err, ErrLeaseWait) {
		reason = "coordination_timeout"
	}
	t.metrics.Fallback(reason)
	response, value, fetchErr := t.fetch(r.Clone(ctx), key, false)
	if fetchErr != nil {
		return sharedResult{}, fetchErr
	}
	defer response.Body.Close()
	if value == nil {
		return sharedResult{degraded: reason}, nil
	}
	// Local fallback publication must not use the unconditional shared writer.
	if t.cache != nil {
		t.cache.Put(key, *value)
	}
	return sharedResult{value: *value, shareable: true, degraded: reason}, nil
}
