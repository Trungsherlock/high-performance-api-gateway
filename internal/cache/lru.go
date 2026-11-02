package cache

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

type Response struct {
	Header    http.Header
	Body      []byte
	ExpiresAt time.Time
}
type entry struct {
	key      string
	response Response
}

// Cache bounds retained body bytes and entry count. Returned values are copies.
type Cache struct {
	mu                  sync.Mutex
	items               map[string]*list.Element
	order               *list.List
	maxItems            int
	maxBytes, usedBytes int64
	now                 func() time.Time
}

func New(maxItems int, maxBytes int64, now func() time.Time) *Cache {
	if maxItems <= 0 || maxBytes <= 0 {
		panic("cache limits must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{items: make(map[string]*list.Element), order: list.New(), maxItems: maxItems, maxBytes: maxBytes, now: now}
}
func copyResponse(r Response) Response {
	r.Header = r.Header.Clone()
	r.Body = append([]byte(nil), r.Body...)
	return r
}
func (c *Cache) remove(e *list.Element) {
	v := e.Value.(entry)
	delete(c.items, v.key)
	c.usedBytes -= int64(len(v.response.Body))
	c.order.Remove(e)
}
func (c *Cache) Get(key string) (Response, bool) {
	c.mu.Lock()
	e, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		return Response{}, false
	}
	r := e.Value.(entry).response
	if !c.now().Before(r.ExpiresAt) {
		c.remove(e)
		c.mu.Unlock()
		return Response{}, false
	}
	c.order.MoveToFront(e)
	c.mu.Unlock()
	return copyResponse(r), true
}
func (c *Cache) Put(key string, r Response) {
	r = copyResponse(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	if int64(len(r.Body)) > c.maxBytes || !c.now().Before(r.ExpiresAt) {
		if e := c.items[key]; e != nil {
			c.remove(e)
		}
		return
	}
	if e := c.items[key]; e != nil {
		// Replacement keeps the existing map slot and list node.
		c.usedBytes -= int64(len(e.Value.(entry).response.Body))
		e.Value = entry{key, r}
		c.order.MoveToFront(e)
	} else {
		c.items[key] = c.order.PushFront(entry{key, r})
	}
	c.usedBytes += int64(len(r.Body))
	for len(c.items) > c.maxItems || c.usedBytes > c.maxBytes {
		c.remove(c.order.Back())
	}
}
