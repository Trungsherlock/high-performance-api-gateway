package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	targets := flag.String("targets", "http://127.0.0.1:8082", "comma-separated destinations, striped per request")
	n := flag.Int("requests", 100, "barrier-start request count")
	id := flag.String("id", "42", "public item query value")
	flag.Parse()
	if *n < 1 || *n > 10000 {
		os.Exit(2)
	}
	urls := strings.Split(*targets, ",")
	tr := &http.Transport{DisableCompression: true, MaxConnsPerHost: *n, MaxIdleConnsPerHost: *n}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	statuses := map[int]int{}
	degradedReasons := map[string]int{}
	rejectionReasons := map[string]int{}
	failures := 0
	var first string
	consistent := true
	for i := 0; i < *n; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			resp, err := client.Get(urls[i%len(urls)] + "/api/public/item?id=" + url.QueryEscape(*id))
			degraded, rejection := "", ""
			status := 0
			body := ""
			if err == nil {
				status = resp.StatusCode
				degraded = resp.Header.Get("X-Gateway-Degraded")
				rejection = resp.Header.Get("X-Gateway-Rejection")
				bytes, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
				resp.Body.Close()
				err = e
				body = string(bytes)
			}
			mu.Lock()
			defer mu.Unlock()
			statuses[status]++
			if degraded != "" {
				degradedReasons[degraded]++
			}
			if rejection != "" {
				rejectionReasons[rejection]++
			}
			if err != nil {
				failures++
			}
			if status == 200 && err == nil {
				if first == "" {
					first = body
				} else if first != body {
					consistent = false
				}
			}
		}(i)
	}
	ready.Wait()
	began := time.Now()
	close(start)
	done.Wait()
	json.NewEncoder(os.Stdout).Encode(map[string]any{"DegradedReasons": degradedReasons, "RejectionReasons": rejectionReasons, "Requests": *n, "StatusCounts": statuses, "TransportErrors": failures, "BodiesConsistent": consistent && first != "", "ElapsedSeconds": time.Since(began).Seconds()})
}
