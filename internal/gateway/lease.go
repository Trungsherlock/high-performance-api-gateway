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
	Protocol     string // empty defaults to v1: read, SET NX, recheck, publish, release
	Duration     time.Duration
	FillTimeout  time.Duration
	WaitTimeout  time.Duration
	PollInterval time.Duration
	Margin       time.Duration
}

func (o LeaseOptions) valid() bool {
	return (o.Degradation == "" || o.Degradation == "protection-first" || o.Degradation == "available-local") && (o.Protocol == "" || o.Protocol == "v1" || o.Protocol == "v2") && o.FillTimeout > 0 && o.Margin > 0 && o.Duration > o.Margin && o.FillTimeout < o.Duration-o.Margin && o.WaitTimeout > 0 && o.PollInterval >= time.Millisecond && o.PollInterval <= time.Second && o.Duration <= time.Minute && o.WaitTimeout <= time.Minute
}

// claimResult is one coordination step: a servable shared value, ownership, or neither.
type claimResult struct {
	value   cache.Response
	hit     bool
	token   string    // set when this caller owns the lease
	start   time.Time // when the acquiring operation began; bounds the owner's fill
	replace bool      // v2: a stored value cannot be served; claim again without reading it
}

// claim costs two operations under v1 (read, then SET NX) and one under v2.
func (t *cacheTransport) claim(ctx context.Context, key string, skipRead bool) (claimResult, error) {
	if t.lease.Protocol != "v2" {
		v, ok, err := t.leaseRead(ctx, key)
		if err != nil || ok {
			return claimResult{value: v, hit: ok}, err
		}
		op := time.Now()
		token, owned, err := t.leases.Acquire(ctx, key, t.lease.Duration)
		t.metrics.Observe("cache_dependency", time.Since(op))
		if err != nil {
			return claimResult{}, t.leaseError(err)
		}
		if !owned {
			token = ""
		}
		return claimResult{token: token, start: op}, nil
	}
	op := time.Now()
	state, v, token, err := t.atomic.ReadOrAcquire(ctx, key, t.lease.Duration, skipRead)
	t.metrics.Observe("cache_dependency", time.Since(op))
	if err != nil {
		return claimResult{}, t.leaseError(err)
	}
	if state == sharedcache.ClaimHit && time.Now().Before(v.ExpiresAt) {
		t.metrics.Add("shared_hits", 1)
		if t.cache != nil {
			t.cache.Put(key, v)
		}
		return claimResult{value: v, hit: true}, nil
	}
	if !skipRead {
		t.metrics.Add("shared_misses", 1)
	}
	switch state {
	case sharedcache.ClaimOwner:
		return claimResult{token: token, start: op}, nil
	case sharedcache.ClaimHit, sharedcache.ClaimUnusable:
		// Without this, waiters would poll an undecodable value until the wait expires.
		return claimResult{replace: true}, nil
	}
	return claimResult{}, nil
}

func (t *cacheTransport) leaseError(err error) error {
	t.dependencyError(err)
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
	skipRead := false
	for {
		if wait.Err() != nil {
			t.metrics.Add("lease_wait_timeouts", 1)
			return sharedResult{}, ErrLeaseWait
		}
		claim, err := t.claim(wait, key, skipRead)
		skipRead = false
		if err != nil {
			if wait.Err() != nil {
				t.metrics.Add("lease_wait_timeouts", 1)
				return sharedResult{}, ErrLeaseWait
			}
			return sharedResult{}, err
		}
		if claim.hit {
			return sharedResult{value: claim.value, shareable: true}, nil
		}
		if claim.token != "" {
			t.metrics.Add("lease_acquired", 1)
			finishWait()
			return t.fillOwned(ctx, r, key, claim.token, claim.start)
		}
		if claim.replace {
			skipRead = true
			continue
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
	acknowledged := false
	defer func() {
		// The publish script deletes the lease, so v2 skips this no-op round trip after
		// an acknowledged publish. Every other exit still releases by token.
		if acknowledged && t.lease.Protocol == "v2" {
			return
		}
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
	// v1 rechecks: another owner may have published just before SET NX. The v2 script
	// read and acquired atomically, so that race cannot happen.
	if t.lease.Protocol != "v2" {
		v, ok, err := t.leaseRead(fill, key)
		if err != nil {
			return sharedResult{}, err
		}
		if ok {
			return sharedResult{value: v, shareable: true}, nil
		}
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
	acknowledged = true
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
