package sharedcache

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadOrAcquireIsAtomic(t *testing.T) {
	r := leaseRedis(t)
	ctx := context.Background()
	key := t.Name() + time.Now().String()
	defer r.client.Del(ctx, Key(key), LeaseKey(key))
	var wg sync.WaitGroup
	claims := make(chan Claim, 20)
	tokens := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, _, token, err := r.ReadOrAcquire(ctx, key, time.Second, false)
			if err != nil {
				t.Error(err)
			}
			claims <- claim
			if claim == ClaimOwner {
				tokens <- token
			}
		}()
	}
	wg.Wait()
	close(claims)
	close(tokens)
	counts := map[Claim]int{}
	for c := range claims {
		counts[c]++
	}
	owner := <-tokens
	if counts[ClaimOwner] != 1 || counts[ClaimWait] != 19 || len(owner) != 64 {
		t.Fatal(counts, owner)
	}
	if ok, err := r.Publish(ctx, key, owner, value()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// A hit returns the value in the same operation and never takes the lease.
	claim, got, token, err := r.ReadOrAcquire(ctx, key, time.Second, false)
	if err != nil || claim != ClaimHit || string(got.Body) != `{"ok":true}` || token != "" {
		t.Fatal(claim, string(got.Body), token, err)
	}
	if r.client.Exists(ctx, LeaseKey(key)).Val() != 0 {
		t.Fatal("hit acquired a lease")
	}
	if _, _, _, err := r.ReadOrAcquire(ctx, key, time.Millisecond/2, false); err == nil {
		t.Fatal("invalid TTL accepted")
	}
}

func TestReadOrAcquireUnusableValues(t *testing.T) {
	r := leaseRedis(t)
	ctx := context.Background()
	for name, raw := range map[string]string{"corrupt": "not-json", "oversize": strings.Repeat("x", MaxValue+1)} {
		t.Run(name, func(t *testing.T) {
			key := t.Name() + time.Now().String()
			defer r.client.Del(ctx, Key(key), LeaseKey(key))
			if err := r.client.Set(ctx, Key(key), raw, 5*time.Second).Err(); err != nil {
				t.Fatal(err)
			}
			claim, _, _, err := r.ReadOrAcquire(ctx, key, time.Second, false)
			if name == "corrupt" {
				// The script cannot decode; the caller must claim again without the read.
				if err != nil || claim != ClaimUnusable || r.client.Exists(ctx, LeaseKey(key)).Val() != 0 {
					t.Fatal(claim, err)
				}
				claim, _, _, err = r.ReadOrAcquire(ctx, key, time.Second, true)
			}
			// Oversize values are never transferred; the script treats them as a miss.
			if err != nil || claim != ClaimOwner {
				t.Fatal(claim, err)
			}
			if claim, _, _, err := r.ReadOrAcquire(ctx, key, time.Second, true); err != nil || claim != ClaimWait {
				t.Fatal("second owner", claim, err)
			}
		})
	}
}

func TestStaleClaimOwnerCannotPublishOrRelease(t *testing.T) {
	r := leaseRedis(t)
	ctx := context.Background()
	key := t.Name() + time.Now().String()
	defer r.client.Del(ctx, Key(key), LeaseKey(key))
	claim, _, old, err := r.ReadOrAcquire(ctx, key, 80*time.Millisecond, false)
	if err != nil || claim != ClaimOwner {
		t.Fatal(claim, err)
	}
	// The old owner never releases; a new owner appears only after expiry.
	var next string
	for end := time.Now().Add(time.Second); time.Now().Before(end) && next == ""; time.Sleep(5 * time.Millisecond) {
		claim, _, token, err := r.ReadOrAcquire(ctx, key, time.Second, false)
		if err != nil {
			t.Fatal(err)
		}
		if claim == ClaimOwner {
			next = token
		}
	}
	if next == "" || next == old {
		t.Fatal("no new owner")
	}
	if ok, err := r.Release(ctx, key, old); err != nil || ok {
		t.Fatal("stale release", ok, err)
	}
	stale := value()
	stale.Body = []byte("old-version")
	if ok, err := r.Publish(ctx, key, old, stale); err != nil || ok {
		t.Fatal("stale publication", ok, err)
	}
	fresh := value()
	fresh.Body = []byte("new-version")
	if ok, err := r.Publish(ctx, key, next, fresh); err != nil || !ok {
		t.Fatal(ok, err)
	}
	claim, got, _, err := r.ReadOrAcquire(ctx, key, time.Second, false)
	if err != nil || claim != ClaimHit || string(got.Body) != "new-version" {
		t.Fatal(claim, string(got.Body), err)
	}
}
