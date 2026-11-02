package coalesce

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestOverlapCancellationAndRetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var g Group[int]
		var count atomic.Int32
		release := make(chan struct{})
		wantErr := errors.New("fetch failed")
		calls := make([]*Call[int], 100)
		for i := range calls {
			calls[i] = g.Start("key", func() (int, error) {
				count.Add(1)
				<-release
				if fail {
					return 0, wantErr
				}
				return 42, nil
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := calls[1].Wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		close(release)
		deadline, stop := context.WithTimeout(context.Background(), time.Second)
		for _, call := range calls {
			v, err := call.Wait(deadline)
			if fail {
				if !errors.Is(err, wantErr) {
					t.Fatal(err)
				}
			} else if err != nil || v != 42 {
				t.Fatalf("%d %v", v, err)
			}
		}
		if count.Load() != 1 {
			t.Fatal(count.Load())
		}
		retry := g.Start("key", func() (int, error) { count.Add(1); return 99, nil })
		v, err := retry.Wait(deadline)
		stop()
		if v != 99 || err != nil || count.Load() != 2 {
			t.Fatal("flight not cleared")
		}
	}
}

func TestDistinctKeysIndependent(t *testing.T) {
	var g Group[int]
	blocked := make(chan struct{})
	first := g.Start("a", func() (int, error) { <-blocked; return 1, nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second := g.Start("b", func() (int, error) { return 2, nil })
	v, err := second.Wait(ctx)
	close(blocked)
	if v != 2 || err != nil {
		t.Fatalf("%d %v", v, err)
	}
	if _, err := first.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
