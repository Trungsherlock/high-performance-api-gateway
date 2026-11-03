package sharedcache

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func leaseRedis(t *testing.T) *Redis {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR to isolated real Redis")
	}
	r, err := New(addr, 200*time.Millisecond, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func TestLeaseAtomicAcquisition(t *testing.T) {
	r := leaseRedis(t)
	ctx := context.Background()
	key := t.Name() + time.Now().String()
	defer r.client.Del(ctx, Key(key), LeaseKey(key))
	var wg sync.WaitGroup
	tokens := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, ok, err := r.Acquire(ctx, key, time.Second)
			if err != nil {
				t.Error(err)
			}
			if ok {
				tokens <- token
			}
		}()
	}
	wg.Wait()
	close(tokens)
	var winners []string
	for token := range tokens {
		winners = append(winners, token)
	}
	if len(winners) != 1 || len(winners[0]) != 64 {
		t.Fatal(winners)
	}
	if ok, err := r.Release(ctx, key, winners[0]); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, ok, err := r.Acquire(ctx, key, time.Millisecond/2); err == nil || ok {
		t.Fatal("invalid TTL", ok, err)
	}
}
func TestStaleOwnerCannotPublishOrRelease(t *testing.T) {
	r := leaseRedis(t)
	ctx := context.Background()
	key := t.Name() + time.Now().String()
	defer r.client.Del(ctx, Key(key), LeaseKey(key))
	old, ok, err := r.Acquire(ctx, key, 80*time.Millisecond)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	// Simulate a paused/crashed owner: no release, then acquire after expiration.
	end := time.Now().Add(time.Second)
	var next string
	for time.Now().Before(end) {
		token, owned, err := r.Acquire(ctx, key, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if owned {
			next = token
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if next == "" || old == next {
		t.Fatal("no new owner")
	}
	if ok, err := r.Release(ctx, key, old); err != nil || ok {
		t.Fatal("stale release", ok, err)
	}
	if got := r.client.Get(ctx, LeaseKey(key)).Val(); got != next {
		t.Fatal("new lease deleted", got)
	}
	v := value()
	v.Body = []byte("old-version")
	if ok, err := r.Publish(ctx, key, old, v); err != nil || ok {
		t.Fatal("stale publication", ok, err)
	}
	if _, hit, err := r.Get(ctx, key); err != nil || hit {
		t.Fatal("old value written", hit, err)
	}
	v.Body = []byte("new-version")
	if ok, err := r.Publish(ctx, key, next, v); err != nil || !ok {
		t.Fatal(ok, err)
	}
	v.Body = []byte("late-old-version")
	if ok, err := r.Publish(ctx, key, old, v); err != nil || ok {
		t.Fatal(ok, err)
	}
	got, hit, err := r.Get(ctx, key)
	if err != nil || !hit || string(got.Body) != "new-version" {
		t.Fatal(fmt.Sprint(got), hit, err)
	}
	if r.client.Exists(ctx, LeaseKey(key)).Val() != 0 {
		t.Fatal("successful publication retained lease")
	}
}
