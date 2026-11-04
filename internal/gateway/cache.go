package gateway

import (
	"bytes"
	"context"
	"errors"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/coalesce"
	"high-performance-api-gateway/internal/observe"
	"high-performance-api-gateway/internal/sharedcache"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var ErrCacheWaiters = errors.New("cache miss client admission full")

type CacheAdmission struct{ MaxFlights, MaxWaiters int }

type cacheTransport struct {
	waiters    chan struct{}
	maxFlights int
	metrics    *observe.Metrics
	next       http.RoundTripper
	cache      *cache.Cache
	shared     sharedcache.Store
	leases     sharedcache.Coordinator
	lease      LeaseOptions
	group      *coalesce.Group[sharedResult]
	timeout    time.Duration
	lifecycle  context.Context
}
type sharedResult struct {
	value     cache.Response
	shareable bool
	degraded  string
}
type replayBody struct {
	io.Reader
	io.Closer
}
type bypassKey struct{}

func (t *cacheTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Context().Value(bypassKey{}) != nil || !cache.Eligible(r) {
		t.metrics.Add("cache_bypass", 1)
		return t.next.RoundTrip(r)
	}
	key := r.URL.String() + "|" + strconv.Quote(r.Header.Get("Accept")) + "|" + strconv.Quote(r.Header.Get("Accept-Encoding")) + "|" + strconv.Quote(r.Header.Get("User-Agent"))
	if value, ok := t.local(key); ok {
		t.metrics.Add("cache_hits", 1)
		return cachedResponse(r, value), nil
	}
	t.metrics.Add("cache_misses", 1)
	if t.group != nil {
		if t.waiters != nil {
			select {
			case t.waiters <- struct{}{}:
			default:
				t.metrics.Add("cache_waiter_rejections", 1)
				return nil, ErrCacheWaiters
			}
			t.metrics.Gauge("cache_waiters", 1)
			defer func() { t.metrics.Gauge("cache_waiters", -1); <-t.waiters }()
		}
		call, follower, admissionErr := t.group.TryStartWithRole(key, t.maxFlights, func() (sharedResult, error) {
			t.metrics.Gauge("flights", 1)
			defer t.metrics.Gauge("flights", -1)
			// Close the miss-to-registration race before issuing any upstream work.
			if value, ok := t.local(key); ok {
				return sharedResult{value: value, shareable: true}, nil
			}
			ctx, cancel := context.WithTimeout(t.lifecycle, t.timeout)
			defer cancel()
			if t.leases != nil {
				return t.withDegradation(ctx, r, key)
			}
			if value, ok := t.lookup(ctx, key); ok {
				return sharedResult{value: value, shareable: true}, nil
			}
			response, value, err := t.fetch(r.Clone(ctx), key, true)
			if err != nil {
				return sharedResult{}, err
			}
			defer response.Body.Close()
			if value != nil {
				return sharedResult{value: *value, shareable: true}, nil
			}
			return sharedResult{}, nil
		})
		if admissionErr != nil {
			t.metrics.Add("cache_flight_rejections", 1)
			return nil, admissionErr
		}
		if follower {
			t.metrics.Add("local_followers", 1)
		}
		result, err := call.Wait(r.Context())
		if err != nil {
			return nil, err
		}
		if result.shareable {
			value := result.value
			value.Header = value.Header.Clone()
			response := cachedResponse(r, value)
			if result.degraded != "" {
				response.Header.Set("X-Gateway-Degraded", result.degraded)
			}
			return response, nil
		}
		// Unshareable responses are fetched per caller under the same limiter.
		response, err := t.next.RoundTrip(r)
		if response != nil && result.degraded != "" {
			response.Header.Set("X-Gateway-Degraded", result.degraded)
		}
		return response, err
	}
	if value, ok := t.lookup(r.Context(), key); ok {
		return cachedResponse(r, value), nil
	}
	response, _, err := t.fetch(r, key, true)
	return response, err
}

func cachedResponse(r *http.Request, value cache.Response) *http.Response {
	seconds := int64(time.Until(value.ExpiresAt) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	value.Header.Set("Cache-Control", "public, max-age="+strconv.FormatInt(seconds, 10))
	value.Header.Del("Age")
	return &http.Response{StatusCode: 200, Header: value.Header, Body: io.NopCloser(bytes.NewReader(value.Body)), ContentLength: int64(len(value.Body)), Request: r}
}

func (t *cacheTransport) fetch(r *http.Request, key string, admit bool) (*http.Response, *cache.Response, error) {
	started := time.Now()
	response, err := t.next.RoundTrip(r)
	if err != nil {
		return response, nil, err
	}
	ttl := cache.Lifetime(response, 10*time.Second)
	// Check policy both before and after removing nominated headers.
	for _, value := range response.Header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			response.Header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		response.Header.Del(name)
	}
	if cache.Lifetime(response, 10*time.Second) <= 0 {
		ttl = 0
	}
	const maxBody = 64 << 10
	if ttl <= 0 || response.ContentLength > maxBody {
		return response, nil, nil
	}
	original := response.Body
	body, readErr := io.ReadAll(io.LimitReader(original, maxBody+1))
	if readErr != nil {
		original.Close()
		return nil, nil, readErr
	}
	if len(body) > maxBody {
		response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
		return response, nil, nil
	}
	original.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if len(response.Trailer) == 0 {
		value := cache.Response{Header: response.Header.Clone(), Body: body, ExpiresAt: started.Add(ttl)}
		if time.Now().Before(value.ExpiresAt) {
			if admit {
				if t.cache != nil {
					t.cache.Put(key, value)
				}
				t.publish(r.Context(), key, value)
			}
			return response, &value, nil
		}
	}
	return response, nil, nil
}

func (t *cacheTransport) local(key string) (cache.Response, bool) {
	if t.cache == nil {
		return cache.Response{}, false
	}
	return t.cache.Get(key)
}
func (t *cacheTransport) lookup(ctx context.Context, key string) (cache.Response, bool) {
	if t.shared == nil {
		return cache.Response{}, false
	}
	start := time.Now()
	value, ok, err := t.shared.Get(ctx, key)
	t.metrics.Observe("cache_dependency", time.Since(start))
	if err != nil {
		t.dependencyError(err)
		t.metrics.Add("shared_read_errors", 1)
		return cache.Response{}, false
	}
	if !ok || !time.Now().Before(value.ExpiresAt) {
		t.metrics.Add("shared_misses", 1)
		return cache.Response{}, false
	}
	t.metrics.Add("shared_hits", 1)
	if t.cache != nil {
		t.cache.Put(key, value)
	}
	return value, true
}
func (t *cacheTransport) publish(ctx context.Context, key string, value cache.Response) {
	if t.shared == nil {
		return
	}
	start := time.Now()
	err := t.shared.Put(ctx, key, value)
	t.metrics.Observe("cache_dependency", time.Since(start))
	if err != nil {
		t.dependencyError(err)
		t.metrics.Add("shared_write_errors", 1)
	} else {
		t.metrics.Add("shared_write_completed", 1)
	}
}

// dependencyError counts one failed shared-cache operation under a fixed cause.
func (t *cacheTransport) dependencyError(err error) {
	t.metrics.Add("dependency_errors", 1)
	t.metrics.Add("dependency_error_"+sharedcache.ErrorKind(err), 1)
}
