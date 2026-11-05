package sharedcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestErrorKind(t *testing.T) {
	for _, c := range []struct {
		err  error
		kind string
	}{
		{fmt.Errorf("%w: %w", ErrPoolWait, context.DeadlineExceeded), "pool_timeout"},
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("lease: %w", context.DeadlineExceeded), "timeout"},
		{&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, "timeout"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "connection"},
		{io.EOF, "connection"},
		{io.ErrUnexpectedEOF, "connection"},
		{redis.ErrClosed, "connection"},
		{net.ErrClosed, "connection"},
		{errors.New("OOM command not allowed when used memory > 'maxmemory'."), "other"},
		{context.Canceled, "other"},
	} {
		if got := ErrorKind(c.err); got != c.kind {
			t.Errorf("%v: got %s, want %s", c.err, got, c.kind)
		}
	}
}

func TestPoolWaitIsDistinct(t *testing.T) {
	r, err := New("127.0.0.1:1", 20*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.slots <- struct{}{} // Another operation holds the only connection; nothing dials.
	defer func() { <-r.slots }()
	start := time.Now()
	if _, _, err := r.Get(context.Background(), "key"); !errors.Is(err, ErrPoolWait) || ErrorKind(err) != "pool_timeout" || time.Since(start) > time.Second {
		t.Fatal(err, time.Since(start))
	}
	for _, op := range []func() error{
		func() error { return r.Put(context.Background(), "key", value()) },
		func() error { _, _, err := r.Acquire(context.Background(), "key", time.Second); return err },
		func() error { _, err := r.Publish(context.Background(), "key", "token", value()); return err },
		func() error { _, err := r.Release(context.Background(), "key", "token"); return err },
	} {
		if err := op(); !errors.Is(err, ErrPoolWait) {
			t.Fatal(err)
		}
	}
}

func TestFailedOperationReleasesSlot(t *testing.T) {
	r, err := New("127.0.0.1:1", 50*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for range 2 {
		if _, _, err := r.Get(context.Background(), "key"); err == nil || errors.Is(err, ErrPoolWait) {
			t.Fatal(err)
		}
	}
	if len(r.slots) != 0 {
		t.Fatal("connection slot leaked")
	}
}
