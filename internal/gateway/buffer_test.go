package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentStreamingBuffers(t *testing.T) {
	payload := func(id string) string { return strings.Repeat("payload-"+id+";", 16384) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload(r.URL.Query().Get("id"))) }))
	defer upstream.Close()
	proxy := testProxy(t, upstream.URL, 5*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			response, err := http.Get(proxy.URL + "/stream?id=" + id)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != payload(id) {
				t.Errorf("stream %s corrupted: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
}
