// Package coalesce shares overlapping work for a key. Results must be immutable.
package coalesce

import (
	"context"
	"errors"
	"sync"
)

var ErrFull = errors.New("cache flight admission full")

type Call[T any] struct {
	done  chan struct{}
	value T
	err   error
}

func (c *Call[T]) Wait(ctx context.Context) (T, error) {
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case <-c.done:
		return c.value, c.err
	}
}

type Group[T any] struct {
	mu    sync.Mutex
	calls map[string]*Call[T]
}

// Start registers synchronously; only the first caller's function is executed.
// The function must bound its own lifetime and must not mutate published results.
func (g *Group[T]) Start(key string, fn func() (T, error)) *Call[T] {
	c, _ := g.StartWithRole(key, fn)
	return c
}

// StartWithRole reports whether the caller joined existing work.
func (g *Group[T]) StartWithRole(key string, fn func() (T, error)) (*Call[T], bool) {
	c, follower, _ := g.TryStartWithRole(key, 0, fn)
	return c, follower
}

// TryStartWithRole bounds registered distinct keys synchronously. Existing keys
// can still join; maxKeys=0 preserves the legacy unlimited registration policy.
func (g *Group[T]) TryStartWithRole(key string, maxKeys int, fn func() (T, error)) (*Call[T], bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.calls[key]; c != nil {
		return c, true, nil
	}
	if maxKeys > 0 && len(g.calls) >= maxKeys {
		return nil, false, ErrFull
	}
	if g.calls == nil {
		g.calls = make(map[string]*Call[T])
	}
	c := &Call[T]{done: make(chan struct{})}
	g.calls[key] = c
	go func() {
		value, err := fn()
		g.mu.Lock()
		defer g.mu.Unlock()
		c.value, c.err = value, err
		delete(g.calls, key)
		close(c.done)
	}()
	return c, false, nil
}
