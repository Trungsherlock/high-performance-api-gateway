package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sample struct {
	scheduled, sent time.Time
	elapsed         time.Duration
	status          int
	failed          bool
	reason          string
	degraded        string
}
type windowResult struct {
	Sent, Success, Rejected, TransportErrors int
	SendP95MS, SendP99MS                     float64
}
type result struct {
	RejectionReasons                                                                           map[string]int
	DegradedReasons                                                                            map[string]int
	Windows                                                                                    map[int]*windowResult
	StartUTC                                                                                   time.Time
	StatusCounts                                                                               map[int]int
	Before, After                                                                              map[string]any
	Phase                                                                                      string
	Offered, Sent, Completed, Success, Rejected, TransportErrors, GeneratorDrops, LateArrivals int
	ElapsedSeconds                                                                             float64
	SendLatencyMS, ScheduledLatencyMS, SchedulingLagMS                                         map[string]float64
}

// keyOptions sizes the V3.3 key spaces; the V1/V2 workloads ignore them.
type keyOptions struct {
	Keys    int     // zipf key-space size
	ZipfS   float64 // zipf exponent; math/rand requires s > 1
	HotKeys int     // hotset size
}

// keySource yields one phase's deterministic key stream from its seed.
type keySource struct {
	workload, phase string
	seed            int64
	seq             int
	rng             *rand.Rand
	zipf            *rand.Zipf
	hotKeys         int
}

func newKeySource(workload, phase string, seed int64, o keyOptions) *keySource {
	k := &keySource{workload: workload, phase: phase, seed: seed, rng: rand.New(rand.NewSource(seed)), hotKeys: o.HotKeys}
	if workload == "zipf" {
		k.zipf = rand.NewZipf(k.rng, o.ZipfS, 1, uint64(o.Keys-1))
	}
	return k
}

// perArrival reports whether keys are drawn for every offered arrival, dropped or
// not, so the stream is independent of timing. V1/V2 workloads keep drawing per
// issued request, which leaves their streams unchanged.
func (k *keySource) perArrival() bool { return k.workload == "zipf" || k.workload == "hotset" }

func (k *keySource) next() string {
	k.seq++
	switch k.workload {
	case "zipf":
		return "z" + strconv.FormatUint(k.zipf.Uint64(), 10)
	case "hotset":
		return "h" + strconv.Itoa(k.rng.Intn(k.hotKeys))
	case "unique":
		return fmt.Sprintf("%s-%d-%d", k.phase, k.seed, k.seq)
	case "mixed":
		if k.rng.Intn(5) == 0 {
			return fmt.Sprintf("%s-%d-%d", k.phase, k.seed, k.seq)
		}
	}
	return "42"
}

// arrivals is the number of scheduled arrivals run() offers in d at rate.
func arrivals(d time.Duration, rate int) int {
	n := 0
	for time.Duration(int64(n)*int64(time.Second)/int64(rate)) < d {
		n++
	}
	return n
}

func percentiles(v []float64) map[string]float64 {
	sort.Float64s(v)
	out := map[string]float64{}
	for _, p := range []int{50, 95, 99, 100} {
		if len(v) > 0 {
			out[fmt.Sprintf("p%d", p)] = v[(len(v)*p+99)/100-1]
		}
	}
	return out
}
func run(ctx context.Context, c *http.Client, target, mode, workload, phase string, d time.Duration, rate, limit int, seed int64, o keyOptions) result {
	start := time.Now()
	end := start.Add(d)
	r := result{Phase: phase, StartUTC: start, StatusCounts: map[int]int{}, RejectionReasons: map[string]int{}, DegradedReasons: map[string]int{}, Windows: map[int]*windowResult{}}
	samples := make(chan sample, limit)
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	collected := make(chan []sample, 1)
	go func() {
		all := make([]sample, 0)
		for s := range samples {
			all = append(all, s)
		}
		collected <- all
	}()
	keys := newKeySource(workload, phase, seed, o)
	issue := func(due time.Time, id string) {
		if id == "" {
			id = keys.next()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			sent := time.Now()
			req, _ := http.NewRequestWithContext(ctx, "GET", target+"/api/public/item?id="+id, nil)
			s := sample{scheduled: due, sent: sent}
			resp, err := c.Do(req)
			if err == nil {
				s.status = resp.StatusCode
				reason := resp.Header.Get("X-Gateway-Rejection")
				if reason == "queue_full" || reason == "wait_timeout" || reason == "coordination_error" || reason == "coordination_timeout" || reason == "lease_lost" || reason == "cache_waiters_full" || reason == "cache_flights_full" {
					s.reason = reason
				}
				degraded := resp.Header.Get("X-Gateway-Degraded")
				if degraded == "dependency_error" || degraded == "coordination_timeout" || degraded == "publication_error" {
					s.degraded = degraded
				}
				_, err = io.Copy(io.Discard, resp.Body)
				e := resp.Body.Close()
				if err == nil {
					err = e
				}
			}
			s.failed = err != nil
			s.elapsed = time.Since(sent)
			samples <- s
		}()
	}
	if mode == "scheduled" {
		for n := int64(0); ; n++ {
			due := start.Add(time.Duration(n * int64(time.Second) / int64(rate)))
			if !due.Before(end) {
				break
			}
			timer := time.NewTimer(time.Until(due))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
			timer.Stop()
			r.Offered++
			id := ""
			if keys.perArrival() {
				id = keys.next()
			}
			if time.Since(due) > time.Second/time.Duration(rate) {
				r.LateArrivals++
			}
			// An arrival due inside the window is sent even when its timer fires after the
			// window ends, like every other late arrival; its lag counts in scheduled
			// latency. Before V3.4 these were dropped, which at thousands of arrivals per
			// second failed a no-drop SLO on timer jitter alone. Only a full in-flight
			// limit (or cancellation) drops an arrival.
			if ctx.Err() != nil {
				r.GeneratorDrops++
				continue
			}
			select {
			case slots <- struct{}{}:
				issue(due, id)
			default:
				r.GeneratorDrops++
			}
		}
	} else {
		for time.Now().Before(end) && ctx.Err() == nil {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				continue
			}
			if !time.Now().Before(end) {
				<-slots
				break
			}
			r.Offered++
			issue(time.Now(), "")
		}
	}
	wg.Wait()
	close(samples)
	all := <-collected
	var send, scheduled, lag []float64
	windowSamples := map[int][]float64{}
	for _, s := range all {
		r.Sent++
		r.Completed++
		r.StatusCounts[s.status]++
		if s.reason != "" {
			r.RejectionReasons[s.reason]++
		}
		if s.degraded != "" {
			r.DegradedReasons[s.degraded]++
		}
		index := int(s.scheduled.Sub(start) / (3 * time.Second))
		win := r.Windows[index]
		if win == nil {
			win = &windowResult{}
			r.Windows[index] = win
		}
		win.Sent++
		if s.failed {
			win.TransportErrors++
		} else if s.status >= 200 && s.status < 300 {
			win.Success++
		}
		if s.status == 503 {
			win.Rejected++
		}
		windowSamples[index] = append(windowSamples[index], float64(s.elapsed)/1e6)
		if s.failed {
			r.TransportErrors++
		} else if s.status >= 200 && s.status < 300 {
			r.Success++
		}
		if s.status == 503 {
			r.Rejected++
		}
		send = append(send, float64(s.elapsed)/1e6)
		scheduled = append(scheduled, float64(s.sent.Sub(s.scheduled)+s.elapsed)/1e6)
		lag = append(lag, float64(s.sent.Sub(s.scheduled))/1e6)
	}
	for index, values := range windowSamples {
		p := percentiles(values)
		r.Windows[index].SendP95MS = p["p95"]
		r.Windows[index].SendP99MS = p["p99"]
	}
	r.ElapsedSeconds = time.Since(start).Seconds()
	r.SendLatencyMS = percentiles(send)
	r.ScheduledLatencyMS = percentiles(scheduled)
	r.SchedulingLagMS = percentiles(lag)
	return r
}
func main() {
	target := flag.String("target", "http://127.0.0.1:8080", "external gateway")
	mode := flag.String("mode", "scheduled", "scheduled or closed")
	work := flag.String("workload", "mixed", "hot, mixed, unique, zipf or hotset")
	keyCount := flag.Int("keys", 10000, "zipf key-space size")
	zipfS := flag.Float64("zipf-s", 1.1, "zipf exponent, greater than 1")
	hotKeys := flag.Int("hot-keys", 100, "hotset size; keys are equally likely")
	planOnly := flag.Bool("plan-only", false, "print each phase's seeded key stream as JSON and send nothing")
	d := flag.Duration("duration", 30*time.Second, "measurement duration")
	warm := flag.Duration("warmup", 5*time.Second, "warmup duration")
	deadline := flag.Duration("timeout", 3*time.Second, "request deadline")
	rate := flag.Int("rate", 500, "scheduled arrivals/sec")
	limit := flag.Int("concurrency", 100, "max in-flight and connections")
	seed := flag.Int64("seed", 42, "workload seed")
	admin := flag.String("admin", "", "optional loopback admin URL for phase snapshots")
	admins := flag.String("admins", "", "comma-separated gateway admin origins for multi-instance snapshots")
	frontAdmin := flag.String("front-admin", "", "front admin origin for routing snapshots")
	backend := flag.String("backend", "", "optional backend URL for phase counters")
	flag.Parse()
	parsed, parseErr := url.Parse(*target)
	if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		fmt.Fprintln(os.Stderr, "invalid target")
		os.Exit(2)
	}
	workloads := map[string]bool{"hot": true, "mixed": true, "unique": true, "zipf": true, "hotset": true}
	if (*mode != "scheduled" && *mode != "closed") || !workloads[*work] || *d <= 0 || *warm < 0 || *deadline <= 0 || *rate <= 0 || *limit <= 0 || *keyCount < 1 || !(*zipfS > 1) || *hotKeys < 1 {
		fmt.Fprintln(os.Stderr, "invalid settings")
		os.Exit(2)
	}
	options := keyOptions{Keys: *keyCount, ZipfS: *zipfS, HotKeys: *hotKeys}
	enc := json.NewEncoder(os.Stdout)
	if *planOnly {
		// The stream a scheduled run offers when no arrival is dropped.
		for _, p := range []struct {
			name string
			d    time.Duration
			seed int64
		}{{"warmup", *warm, *seed}, {"measurement", *d, *seed + 1}} {
			if p.d <= 0 {
				continue
			}
			keys := newKeySource(*work, p.name, p.seed, options)
			stream := make([]string, arrivals(p.d, *rate))
			for i := range stream {
				stream[i] = keys.next()
			}
			_ = enc.Encode(map[string]any{"Phase": p.name, "Seed": p.seed, "Workload": *work, "Rate": *rate, "DurationSeconds": p.d.Seconds(), "Keys": stream})
		}
		return
	}
	tr := &http.Transport{MaxConnsPerHost: *limit, MaxIdleConnsPerHost: *limit, DisableCompression: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: *deadline}
	snapshot := func() map[string]any {
		out := map[string]any{}
		endpoints := map[string]string{}
		if *admin != "" {
			endpoints["gateway"] = *admin + "/metrics"
		}
		if *backend != "" {
			endpoints["backend"] = *backend + "/stats"
		}
		if *frontAdmin != "" {
			endpoints["front"] = *frontAdmin + "/metrics"
		}
		if *admins != "" {
			for i, base := range strings.Split(*admins, ",") {
				endpoints[fmt.Sprintf("gateway_%d", i+1)] = base + "/metrics"
			}
		}
		for name, url := range endpoints {
			resp, err := c.Get(url)
			if err != nil {
				out[name+"_error"] = err.Error()
				continue
			}
			var v any
			err = json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if err != nil {
				out[name+"_error"] = err.Error()
			} else {
				out[name] = v
			}
		}
		return out
	}
	phase := func(name string, duration time.Duration, seed int64) {
		before := snapshot()
		r := run(context.Background(), c, *target, *mode, *work, name, duration, *rate, *limit, seed, options)
		r.Before = before
		r.After = snapshot()
		_ = enc.Encode(r)
	}
	if *warm > 0 {
		phase("warmup", *warm, *seed)
	}
	phase("measurement", *d, *seed+1)
}
