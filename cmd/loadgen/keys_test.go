package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

var defaultKeys = keyOptions{Keys: 10000, ZipfS: 1.1, HotKeys: 100}

// The V1/V2 streams must not change: V3 keeps mixed as the no-reuse control.
func TestLegacyStreamsUnchanged(t *testing.T) {
	for _, workload := range []string{"hot", "mixed", "unique"} {
		rng := rand.New(rand.NewSource(43))
		keys := newKeySource(workload, "measurement", 43, defaultKeys)
		for seq := 1; seq <= 5000; seq++ {
			want := "42"
			if workload == "unique" || (workload == "mixed" && rng.Intn(5) == 0) {
				want = fmt.Sprintf("measurement-43-%d", seq)
			}
			if got := keys.next(); got != want {
				t.Fatal(workload, seq, got, want)
			}
		}
	}
}

func TestReuseStreamsAreSeededAndShaped(t *testing.T) {
	const draws = 200000
	for _, workload := range []string{"zipf", "hotset"} {
		a, b := newKeySource(workload, "warmup", 7, defaultKeys), newKeySource(workload, "measurement", 7, defaultKeys)
		counts := map[string]int{}
		for i := 0; i < draws; i++ {
			k := a.next()
			if k != b.next() {
				t.Fatal(workload, "stream depends on more than the seed", i)
			}
			counts[k]++
		}
		if !a.perArrival() {
			t.Fatal(workload, "keys must be drawn per offered arrival")
		}
		if workload == "hotset" {
			// Equally likely keys: each of 100 expects 2,000 draws.
			for i := 0; i < 100; i++ {
				if n := counts[fmt.Sprint("h", i)]; n < 1700 || n > 2300 {
					t.Fatal("hotset key", i, n)
				}
			}
			if len(counts) != 100 {
				t.Fatal("hotset keys", len(counts))
			}
			continue
		}
		// P(rank r) = (r+1)^-s / H; check the top rank and that ranks decrease.
		h := 0.0
		for r := 1; r <= 10000; r++ {
			h += math.Pow(float64(r), -1.1)
		}
		if p := float64(counts["z0"]) / draws; math.Abs(p-1/h) > 0.01 {
			t.Fatal("zipf top-rank share", p, 1/h)
		}
		if counts["z0"] <= counts["z1"] || counts["z1"] <= counts["z9"] || counts["z9"] <= counts["z999"] {
			t.Fatal("zipf ranks not decreasing", counts["z0"], counts["z1"], counts["z9"], counts["z999"])
		}
		for k := range counts {
			var r int
			if _, err := fmt.Sscanf(k, "z%d", &r); err != nil || r < 0 || r >= 10000 {
				t.Fatal("zipf key out of range", k)
			}
		}
	}
}

// At a rate far above timer resolution, the last arrivals' timers fire after the window
// ends; they are still sent, so only a full in-flight limit drops arrivals.
func TestLateFinalArrivalsAreSent(t *testing.T) {
	var mu sync.Mutex
	received := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received++
		mu.Unlock()
	}))
	defer s.Close()
	// Pooled connections, as main configures them; requests beyond the pool wait for a
	// connection while holding their in-flight slot, which the limit below covers.
	tr := &http.Transport{MaxConnsPerHost: 64, MaxIdleConnsPerHost: 64}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	const rate, window = 20000, 100 * time.Millisecond
	planned := arrivals(window, rate)
	for attempt := 0; attempt < 5; attempt++ {
		mu.Lock()
		received = 0
		mu.Unlock()
		r := run(context.Background(), client, s.URL, "scheduled", "zipf", "measurement", window, rate, planned, 43, defaultKeys)
		if r.Offered != planned || r.GeneratorDrops != 0 || r.Sent != planned || r.TransportErrors != 0 || received != planned {
			t.Fatal(attempt, r.Offered, r.Sent, r.GeneratorDrops, r.TransportErrors, received, planned)
		}
	}
}

// A scheduled run sends exactly the planned keys; drops skip keys without shifting later ones.
func TestScheduledRunFollowsPlan(t *testing.T) {
	for _, slow := range []bool{false, true} {
		var mu sync.Mutex
		var received []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slow {
				time.Sleep(20 * time.Millisecond)
			}
			mu.Lock()
			received = append(received, r.URL.Query().Get("id"))
			mu.Unlock()
		}))
		limit := 64
		if slow {
			limit = 1
		}
		r := run(context.Background(), s.Client(), s.URL, "scheduled", "zipf", "measurement", 200*time.Millisecond, 500, limit, 43, defaultKeys)
		s.Close()
		keys := newKeySource("zipf", "measurement", 43, defaultKeys)
		plan := map[string]int{}
		for i := 0; i < arrivals(200*time.Millisecond, 500); i++ {
			plan[keys.next()]++
		}
		if r.Offered != arrivals(200*time.Millisecond, 500) || len(received) != r.Sent || (slow && r.GeneratorDrops == 0) || (!slow && r.GeneratorDrops != 0) {
			t.Fatal(slow, r.Offered, r.Sent, r.GeneratorDrops, len(received))
		}
		sort.Strings(received)
		for _, k := range received {
			if plan[k] == 0 {
				t.Fatal("unplanned key", k)
			}
			plan[k]--
		}
	}
}
