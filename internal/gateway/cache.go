package gateway

import (
	"bytes"
	"context"
	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/coalesce"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type cacheTransport struct {
	next      http.RoundTripper
	cache     *cache.Cache
	group     *coalesce.Group[sharedResult]
	timeout   time.Duration
	lifecycle context.Context
}
type sharedResult struct {
	value     cache.Response
	shareable bool
}
type replayBody struct {
	io.Reader
	io.Closer
}
type bypassKey struct{}

func (t *cacheTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Context().Value(bypassKey{}) != nil || !cache.Eligible(r) {
		return t.next.RoundTrip(r)
	}
	key := r.URL.String() + "|" + strconv.Quote(r.Header.Get("Accept")) + "|" + strconv.Quote(r.Header.Get("Accept-Encoding")) + "|" + strconv.Quote(r.Header.Get("User-Agent"))
	if value, ok := t.cache.Get(key); ok {
		return cachedResponse(r, value), nil
	}
	if t.group != nil {
		call := t.group.Start(key, func() (sharedResult, error) {
			// Close the miss-to-registration race before issuing any upstream work.
			if value, ok := t.cache.Get(key); ok {
				return sharedResult{value, true}, nil
			}
			ctx, cancel := context.WithTimeout(t.lifecycle, t.timeout)
			defer cancel()
			response, err := t.fetch(r.Clone(ctx), key)
			if err != nil {
				return sharedResult{}, err
			}
			defer response.Body.Close()
			if value, ok := t.cache.Get(key); ok {
				return sharedResult{value, true}, nil
			}
			return sharedResult{}, nil
		})
		result, err := call.Wait(r.Context())
		if err != nil {
			return nil, err
		}
		if result.shareable {
			value := result.value
			value.Header = value.Header.Clone()
			return cachedResponse(r, value), nil
		}
		// Unshareable responses are fetched per caller under the same limiter.
		return t.next.RoundTrip(r)
	}
	return t.fetch(r, key)
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

func (t *cacheTransport) fetch(r *http.Request, key string) (*http.Response, error) {
	started := time.Now()
	response, err := t.next.RoundTrip(r)
	if err != nil {
		return response, err
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
		return response, nil
	}
	original := response.Body
	body, readErr := io.ReadAll(io.LimitReader(original, maxBody+1))
	if readErr != nil {
		original.Close()
		return nil, readErr
	}
	if len(body) > maxBody {
		response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
		return response, nil
	}
	original.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if len(response.Trailer) == 0 {
		t.cache.Put(key, cache.Response{Header: response.Header, Body: body, ExpiresAt: started.Add(ttl)})
	}
	return response, nil
}
