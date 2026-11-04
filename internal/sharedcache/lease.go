package sharedcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"high-performance-api-gateway/internal/cache"
	"strings"
	"time"
)

var ErrUnshareable = errors.New("response cannot be encoded for shared storage")

// Coordinator uses one Redis authority. Ownership is temporary, never exactly-once.
type Coordinator interface {
	Store
	Acquire(context.Context, string, time.Duration) (token string, acquired bool, err error)
	Publish(context.Context, string, string, cache.Response) (bool, error)
	Release(context.Context, string, string) (bool, error)
}

func LeaseKey(key string) string {
	return strings.Replace(Key(key), "gateway:response:v1:", "gateway:lease:v1:", 1)
}
func (r *Redis) Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	if ttl < time.Millisecond {
		return "", false, errors.New("lease TTL must be at least one millisecond")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false, err
	}
	token := hex.EncodeToString(raw[:])
	ctx, done, err := r.bound(ctx)
	if err != nil {
		return "", false, err
	}
	defer done()
	ok, err := r.client.SetNX(ctx, LeaseKey(key), token, ttl).Result()
	if !ok || err != nil {
		return "", false, err
	}
	return token, true, nil
}

// SET must succeed before DEL. Redis script errors do not roll back earlier writes.
const ownedPublish = `if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end; local t=redis.call('TIME'); local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000); if tonumber(ARGV[3])<=now then return 0 end; redis.call('SET',KEYS[2],ARGV[2],'PXAT',ARGV[3]); redis.call('DEL',KEYS[1]); return 1`
const ownedRelease = `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end; return 0`

func (r *Redis) Publish(ctx context.Context, key, token string, v cache.Response) (bool, error) {
	if token == "" {
		return false, errors.New("empty lease token")
	}
	raw, expiry, err := encode(v)
	if err != nil {
		return false, err
	}
	if raw == nil {
		return false, ErrUnshareable
	}
	ctx, done, err := r.bound(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	n, err := r.client.Eval(ctx, ownedPublish, []string{LeaseKey(key), Key(key)}, token, raw, expiry.UnixMilli()).Int()
	return n == 1, err
}
func (r *Redis) Release(ctx context.Context, key, token string) (bool, error) {
	if token == "" {
		return false, errors.New("empty lease token")
	}
	ctx, done, err := r.bound(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	n, err := r.client.Eval(ctx, ownedRelease, []string{LeaseKey(key)}, token).Int()
	return n == 1, err
}
