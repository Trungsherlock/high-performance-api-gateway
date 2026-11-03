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
}
type windowResult struct {
	Sent, Success, Rejected, TransportErrors int
	SendP95MS, SendP99MS                     float64
}
type result struct {
	RejectionReasons                                                                           map[string]int
	Windows                                                                                    map[int]*windowResult
	StartUTC                                                                                   time.Time
	StatusCounts                                                                               map[int]int
	Before, After                                                                              map[string]any
	Phase                                                                                      string
	Offered, Sent, Completed, Success, Rejected, TransportErrors, GeneratorDrops, LateArrivals int
	ElapsedSeconds                                                                             float64
	SendLatencyMS, ScheduledLatencyMS, SchedulingLagMS                                         map[string]float64
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
func run(ctx context.Context, c *http.Client, target, mode, workload, phase string, d time.Duration, rate, limit int, seed int64) result {
	start := time.Now()
	end := start.Add(d)
	r := result{Phase: phase, StartUTC: start, StatusCounts: map[int]int{}, RejectionReasons: map[string]int{}, Windows: map[int]*windowResult{}}
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
	rng := rand.New(rand.NewSource(seed))
	seq := 0
	issue := func(due time.Time) {
		id := "42"
		seq++
		if workload == "unique" || (workload == "mixed" && rng.Intn(5) == 0) {
			id = fmt.Sprintf("%s-%d-%d", phase, seed, seq)
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
				if reason == "queue_full" || reason == "wait_timeout" {
					s.reason = reason
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
			if time.Since(due) > time.Second/time.Duration(rate) {
				r.LateArrivals++
			}
			if ctx.Err() != nil || !time.Now().Before(end) {
				r.GeneratorDrops++
				continue
			}
			select {
			case slots <- struct{}{}:
				issue(due)
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
			issue(time.Now())
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
	work := flag.String("workload", "mixed", "hot, mixed, or unique")
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
	if (*mode != "scheduled" && *mode != "closed") || (*work != "hot" && *work != "mixed" && *work != "unique") || *d <= 0 || *warm < 0 || *deadline <= 0 || *rate <= 0 || *limit <= 0 {
		fmt.Fprintln(os.Stderr, "invalid settings")
		os.Exit(2)
	}
	tr := &http.Transport{MaxConnsPerHost: *limit, MaxIdleConnsPerHost: *limit, DisableCompression: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: *deadline}
	enc := json.NewEncoder(os.Stdout)
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
		r := run(context.Background(), c, *target, *mode, *work, name, duration, *rate, *limit, seed)
		r.Before = before
		r.After = snapshot()
		_ = enc.Encode(r)
	}
	if *warm > 0 {
		phase("warmup", *warm, *seed)
	}
	phase("measurement", *d, *seed+1)
}
