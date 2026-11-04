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

// Claim is the outcome of one ReadOrAcquire call.
type Claim int

const (
	ClaimWait     Claim = iota // another owner holds the lease
	ClaimHit                   // a servable value was returned
	ClaimOwner                 // this caller now holds the lease
	ClaimUnusable              // a value is stored but cannot be served; claim again with skipRead
)

// AtomicCoordinator adds lease protocol v2: one script either returns the stored
// response or acquires its lease, so no recheck after acquisition is needed.
type AtomicCoordinator interface {
	Coordinator
	ReadOrAcquire(ctx context.Context, key string, ttl time.Duration, skipRead bool) (Claim, cache.Response, string, error)
}

func LeaseKey(key string) string {
	return strings.Replace(Key(key), "gateway:response:v1:", "gateway:lease:v1:", 1)
}
func newToken(ttl time.Duration) (string, error) {
	if ttl < time.Millisecond {
		return "", errors.New("lease TTL must be at least one millisecond")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
func (r *Redis) Acquire(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	token, err := newToken(ttl)
	if err != nil {
		return "", false, err
	}
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

// Same size and PTTL checks as lookup, then SET NX PX in the same script: {1,value,pttl}
// is a hit, {2} means this caller owns the lease, {0} means another owner does.
// ARGV[4]=1 skips the read so an owner can replace a value that could not be decoded.
const readOrAcquire = `if ARGV[4]~='1' then local n=redis.call('STRLEN',KEYS[1]); if n>0 and n<=tonumber(ARGV[1]) then local t=redis.call('PTTL',KEYS[1]); if t>0 then return {1,redis.call('GET',KEYS[1]),t} end end end; if redis.call('SET',KEYS[2],ARGV[2],'NX','PX',ARGV[3]) then return {2} end; return {0}`

func (r *Redis) ReadOrAcquire(ctx context.Context, key string, ttl time.Duration, skipRead bool) (Claim, cache.Response, string, error) {
	token, err := newToken(ttl)
	if err != nil {
		return ClaimWait, cache.Response{}, "", err
	}
	skip := "0"
	if skipRead {
		skip = "1"
	}
	start := time.Now()
	ctx, done, err := r.bound(ctx)
	if err != nil {
		return ClaimWait, cache.Response{}, "", err
	}
	defer done()
	values, err := r.client.Eval(ctx, readOrAcquire, []string{Key(key), LeaseKey(key)}, MaxValue, token, ttl.Milliseconds(), skip).Slice()
	if err != nil {
		return ClaimWait, cache.Response{}, "", err
	}
	var kind int64 = -1 // A failed assertion would otherwise read as kind 0 (wait).
	if len(values) > 0 {
		if k, ok := values[0].(int64); ok {
			kind = k
		}
	}
	switch {
	case kind == 0 && len(values) == 1:
		return ClaimWait, cache.Response{}, "", nil
	case kind == 2 && len(values) == 1:
		return ClaimOwner, cache.Response{}, token, nil
	case kind == 1 && len(values) == 3:
		raw, okRaw := values[1].(string)
		ms, okTTL := values[2].(int64)
		if !okRaw || !okTTL {
			return ClaimWait, cache.Response{}, "", errors.New("invalid Redis reply types")
		}
		if v, hit, _ := decode([]byte(raw), ms, start); hit {
			return ClaimHit, v, "", nil
		}
		return ClaimUnusable, cache.Response{}, "", nil
	}
	return ClaimWait, cache.Response{}, "", errors.New("invalid Redis reply")
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
