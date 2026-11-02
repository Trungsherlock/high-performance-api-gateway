package cache

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestLRUAndBudgets(t *testing.T) {
	now := time.Now()
	c := New(2, 5, func() time.Time { return now })
	put := func(k, b string) {
		c.Put(k, Response{Header: http.Header{"X-Test": {"original"}}, Body: []byte(b), ExpiresAt: now.Add(time.Second)})
	}
	put("a", "aa")
	put("b", "bb")
	c.Get("a")
	put("c", "cc")
	if _, ok := c.Get("b"); ok {
		t.Fatal("LRU entry survived")
	}
	put("a", "aaaa")
	if c.usedBytes != 4 || len(c.items) != 1 {
		t.Fatal("overwrite/byte eviction accounting incorrect")
	}
	put("a", "x")
	if c.usedBytes != 1 {
		t.Fatal("overwrite did not subtract old bytes")
	}
	now = now.Add(time.Second)
	if _, ok := c.Get("a"); ok || c.usedBytes != 0 {
		t.Fatal("expired entry survived")
	}
	put("big", "123456")
	if len(c.items) != 0 {
		t.Fatal("oversized entry stored")
	}
}
func TestCopies(t *testing.T) {
	c := New(2, 100, nil)
	original := Response{Header: http.Header{"X-Test": {"safe"}}, Body: []byte("safe"), ExpiresAt: time.Now().Add(time.Hour)}
	c.Put("key", original)
	original.Body[0] = 'X'
	original.Header.Set("X-Test", "bad")
	result, _ := c.Get("key")
	result.Body[0] = 'X'
	result.Header.Set("X-Test", "bad")
	again, _ := c.Get("key")
	if string(again.Body) != "safe" || again.Header.Get("X-Test") != "safe" {
		t.Fatal("mutable cache data escaped")
	}
}
func TestConcurrent(t *testing.T) {
	c := New(20, 100, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				key := fmt.Sprint(i, n%4)
				c.Put(key, Response{Body: []byte("hello"), ExpiresAt: time.Now().Add(time.Minute)})
				c.Get(key)
			}
		}(i)
	}
	wg.Wait()
	var total int64
	for key, e := range c.items {
		v := e.Value.(entry)
		if key != v.key {
			t.Fatal("map/list mismatch")
		}
		total += int64(len(v.response.Body))
	}
	if total != c.usedBytes || total > 100 || len(c.items) > 20 || c.order.Len() != len(c.items) {
		t.Fatal("cache invariant broken")
	}
}
