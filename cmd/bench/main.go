// bench is a closed-loop local experiment harness, not a production load generator.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"high-performance-api-gateway/internal/cache"
	"high-performance-api-gateway/internal/gateway"
	"high-performance-api-gateway/internal/mockbackend"
)

type result struct {
	Mode         string  `json:"mode"`
	Scenario     string  `json:"scenario"`
	Trial        int     `json:"trial"`
	Go           string  `json:"go"`
	OS           string  `json:"os"`
	CPUs         int     `json:"cpus"`
	Concurrency  int     `json:"concurrency"`
	Seconds      float64 `json:"seconds"`
	Requests     int     `json:"requests"`
	Success      int     `json:"success"`
	Errors       int     `json:"errors"`
	Rejected     int     `json:"rejected"`
	RPS          float64 `json:"successful_rps"`
	P50          float64 `json:"p50_ms"`
	P95          float64 `json:"p95_ms"`
	P99          float64 `json:"p99_ms"`
	BackendCalls uint64  `json:"backend_calls"`
	CPUSeconds   float64 `json:"process_cpu_seconds"`
	RSS          uint64  `json:"process_rss_bytes"`
}

func main() {
	mode := flag.String("mode", "full", "direct, proxy, cache, or full")
	scenario := flag.String("scenario", "hot", "hot, mixed, cold, or expiry")
	trials := flag.Int("trials", 3, "independent trials")
	concurrency := flag.Int("concurrency", 100, "closed-loop workers")
	duration := flag.Duration("duration", 3*time.Second, "measurement duration")
	warmup := flag.Duration("warmup", 300*time.Millisecond, "unmeasured warmup for steady load")
	delay := flag.Duration("delay", 50*time.Millisecond, "backend service delay")
	cpu := flag.String("cpuprofile", "", "CPU profile output")
	heap := flag.String("memprofile", "", "heap profile output")
	flag.Parse()
	if *trials < 1 || *concurrency < 1 || *duration <= 0 || *warmup < 0 || *delay < 0 {
		log.Fatal("invalid benchmark parameters")
	}
	if *mode != "direct" && *mode != "proxy" && *mode != "cache" && *mode != "full" {
		log.Fatal("invalid mode")
	}
	if *scenario != "hot" && *scenario != "mixed" && *scenario != "cold" && *scenario != "expiry" {
		log.Fatal("invalid scenario")
	}
	if *cpu != "" {
		f, err := os.Create(*cpu)
		if err != nil {
			log.Fatal(err)
		}
		if err = pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		defer pprof.StopCPUProfile()
	}
	for trial := 1; trial <= *trials; trial++ {
		r := run(*mode, *scenario, trial, *concurrency, *duration, *warmup, *delay)
		if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
			log.Fatal(err)
		}
	}
	if *heap != "" {
		runtime.GC()
		f, err := os.Create(*heap)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		if err = pprof.WriteHeapProfile(f); err != nil {
			log.Fatal(err)
		}
	}
}
func run(mode, scenario string, trial, workers int, duration, warmup, delay time.Duration) result {
	backend := httptest.NewServer(mockbackend.New(delay))
	defer backend.Close()
	target := backend.URL
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()
	if mode != "direct" {
		var storage *cache.Cache
		if mode != "proxy" {
			storage = cache.New(1000, 16<<20, nil)
		}
		options := gateway.Options{Lifecycle: lifecycle}
		if mode == "full" {
			options.Coalesce = true
			options.MaxConcurrent = 32
			options.PermitWait = 50 * time.Millisecond
		}
		h, tr, err := gateway.NewWithOptions(backend.URL, 2*time.Second, storage, options)
		if err != nil {
			log.Fatal(err)
		}
		defer tr.CloseIdleConnections()
		server := httptest.NewServer(h)
		defer server.Close()
		target = server.URL
	}
	tr := &http.Transport{DisableCompression: true, MaxIdleConns: workers * 2, MaxIdleConnsPerHost: workers, MaxConnsPerHost: workers}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	var sequence atomic.Uint64
	request := func(id uint64) (int, error) {
		resp, err := client.Get(fmt.Sprintf("%s/api/public/item?id=%d", target, id))
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, err
	}
	calls := func() uint64 {
		resp, err := client.Get(backend.URL + "/stats")
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()
		var s struct {
			Calls uint64 `json:"calls"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&s); err != nil {
			log.Fatal(err)
		}
		return s.Calls
	}
	if scenario == "hot" || scenario == "expiry" {
		if _, err := request(42); err != nil {
			log.Fatal(err)
		}
	}
	exercise := func(d time.Duration, record bool) ([]time.Duration, int, int, int) {
		end := time.Now().Add(d)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var latencies []time.Duration
		success, errs, rejected := 0, 0, 0
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var local []time.Duration
				s, e, reject := 0, 0, 0
				for n := 0; ; n++ {
					if scenario == "cold" || scenario == "expiry" {
						if n > 0 {
							break
						}
					} else if !time.Now().Before(end) {
						break
					}
					id := uint64(42)
					if scenario == "mixed" {
						v := sequence.Add(1)
						if v%5 == 0 {
							id = v + 1000
						}
					}
					began := time.Now()
					status, err := request(id)
					if record {
						local = append(local, time.Since(began))
						if err != nil || status < 200 || status >= 300 {
							e++
						} else {
							s++
						}
						if status == 503 {
							reject++
						}
					}
				}
				mu.Lock()
				latencies = append(latencies, local...)
				success += s
				errs += e
				rejected += reject
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		return latencies, success, errs, rejected
	}
	if scenario == "hot" || scenario == "mixed" {
		exercise(warmup, false)
	}
	if scenario == "expiry" {
		time.Sleep(10100 * time.Millisecond)
	}
	before := calls()
	cpuBefore, _ := processUsage()
	began := time.Now()
	latencies, success, errs, rejected := exercise(duration, true)
	elapsed := time.Since(began)
	cpuAfter, rss := processUsage()
	after := calls()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		i := int(math.Ceil(p*float64(len(latencies)))) - 1
		return float64(latencies[i]) / float64(time.Millisecond)
	}
	return result{Mode: mode, Scenario: scenario, Trial: trial, Go: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH, CPUs: runtime.NumCPU(), Concurrency: workers, Seconds: elapsed.Seconds(), Requests: len(latencies), Success: success, Errors: errs, Rejected: rejected, RPS: float64(success) / elapsed.Seconds(), P50: percentile(.5), P95: percentile(.95), P99: percentile(.99), BackendCalls: after - before, CPUSeconds: cpuAfter - cpuBefore, RSS: rss}
}
