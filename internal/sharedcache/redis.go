// Package sharedcache implements the optional, single-Redis response tier.
package sharedcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"high-performance-api-gateway/internal/cache"
)

const MaxValue = 96 << 10 // Includes JSON/base64 body, headers and metadata.
const MaxBody = 64 << 10
const MaxLifetime = 10 * time.Second

// Store returns misses with ok=false and dependency failures with a non-nil error.
// Returned expiry must already account for lookup time. Implementations bound operations.
type Store interface {
	Get(context.Context, string) (cache.Response, bool, error)
	Put(context.Context, string, cache.Response) error
}

type Redis struct {
	client  *redis.Client
	timeout time.Duration
}
type envelope struct {
	Version          int         `json:"version"`
	ExpiresUnixMilli int64       `json:"expires_unix_milli"`
	Header           http.Header `json:"header"`
	Body             []byte      `json:"body"`
}

func New(addr string, timeout time.Duration, pool int) (*Redis, error) {
	if addr == "" || timeout <= 0 || pool < 1 || pool > 256 {
		return nil, fmt.Errorf("invalid Redis address, timeout or pool size")
	}
	c := redis.NewClient(&redis.Options{Addr: addr, Protocol: 2, MaxRetries: -1, DialTimeout: timeout, ReadTimeout: timeout, WriteTimeout: timeout, PoolTimeout: timeout, ContextTimeoutEnabled: true, PoolSize: pool, MaxActiveConns: pool, MaxIdleConns: pool, DisableIdentity: true})
	return &Redis{c, timeout}, nil
}
func (r *Redis) Close() error { return r.client.Close() }
func Key(key string) string {
	h := sha256.Sum256([]byte(key))
	return "gateway:response:v1:" + hex.EncodeToString(h[:])
}

// Read size, value and PTTL atomically. Never transfer an arbitrarily large value.
const lookup = `local n=redis.call('STRLEN',KEYS[1]); if n==0 or n>tonumber(ARGV[1]) then return {} end; local t=redis.call('PTTL',KEYS[1]); if t<=0 then return {} end; return {redis.call('GET',KEYS[1]),t}`

func (r *Redis) Get(ctx context.Context, key string) (cache.Response, bool, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	values, err := r.client.Eval(ctx, lookup, []string{Key(key)}, MaxValue).Slice()
	if err != nil {
		return cache.Response{}, false, err
	}
	if len(values) == 0 {
		return cache.Response{}, false, nil
	}
	if len(values) != 2 {
		return cache.Response{}, false, errors.New("invalid Redis reply")
	}
	raw, ok := values[0].(string)
	ms, okTTL := values[1].(int64)
	if !ok || !okTTL {
		return cache.Response{}, false, errors.New("invalid Redis reply types")
	}
	return decode([]byte(raw), ms, start)
}
func decode(raw []byte, ms int64, start time.Time) (cache.Response, bool, error) {
	if len(raw) > MaxValue || ms <= 0 {
		return cache.Response{}, false, nil
	}
	var v envelope
	if json.Unmarshal(raw, &v) != nil || v.Version != 1 || len(v.Body) > MaxBody || !safeHeaders(v.Header) {
		return cache.Response{}, false, nil
	}
	ttl := time.Duration(ms) * time.Millisecond
	if ms > int64(MaxLifetime/time.Millisecond) {
		ttl = MaxLifetime
	}
	policy := cache.Lifetime(&http.Response{StatusCode: 200, Header: v.Header}, MaxLifetime)
	if policy <= 0 {
		return cache.Response{}, false, nil
	}
	if ttl > policy {
		ttl = policy
	}
	// start + remaining TTL subtracts the entire operation, including pool wait.
	expiry := start.Add(ttl)
	original := time.UnixMilli(v.ExpiresUnixMilli)
	if expiry.After(original) {
		expiry = original
	}
	if !time.Now().Before(expiry) {
		return cache.Response{}, false, nil
	}
	return cache.Response{Header: v.Header, Body: v.Body, ExpiresAt: expiry}, true, nil
}
func safeHeaders(h http.Header) bool {
	size, count := 0, 0
	for name, values := range h {
		if !allowed(name) {
			return false
		}
		for _, v := range values {
			size += len(name) + len(v)
			count++
			if size > 8192 || count > 128 {
				return false
			}
			for _, c := range v {
				if (c < 32 && c != '\t') || c == 127 {
					return false
				}
			}
		}
	}
	return true
}
func allowed(name string) bool {
	switch name {
	case "Cache-Control", "Content-Type", "Content-Language", "Etag", "Last-Modified", "Date", "Expires", "Age":
		return true
	}
	return false
}
func encode(v cache.Response) ([]byte, time.Time, error) {
	if len(v.Body) > MaxBody {
		return nil, time.Time{}, nil
	}
	policy := cache.Lifetime(&http.Response{StatusCode: 200, Header: v.Header}, MaxLifetime)
	if policy <= 0 {
		return nil, time.Time{}, nil
	}
	h := make(http.Header)
	size, count := 0, 0
	for k, values := range v.Header {
		if allowed(k) {
			for _, v := range values {
				size += len(k) + len(v)
				count++
				if size > 8192 || count > 128 {
					return nil, time.Time{}, nil
				}
			}
			h[k] = append([]string(nil), values...)
		}
	}
	if !safeHeaders(h) {
		return nil, time.Time{}, nil
	}
	raw, err := json.Marshal(envelope{Version: 1, ExpiresUnixMilli: v.ExpiresAt.UnixMilli(), Header: h, Body: v.Body})
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(raw) > MaxValue {
		return nil, time.Time{}, nil
	}
	// Absolute expiry prevents pool/network delay from restarting the fill lifetime.
	expiry := v.ExpiresAt
	capExpiry := time.Now().Add(policy)
	if expiry.After(capExpiry) {
		expiry = capExpiry
	}
	if time.Until(expiry) < time.Millisecond {
		return nil, time.Time{}, nil
	}
	return raw, expiry, nil
}

func (r *Redis) Put(ctx context.Context, key string, v cache.Response) error {
	raw, expiry, err := encode(v)
	if err != nil || raw == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.client.Do(ctx, "SET", Key(key), raw, "PXAT", expiry.UnixMilli()).Err()
}
