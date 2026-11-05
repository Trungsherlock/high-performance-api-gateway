# V2.0-V2.1 results

Recorded 2026-11-02 (America/Chicago). These are local experiments, not production
capacity claims. Reproduction and metric definitions are in the [V2 guide](README.md).
The [canonical machine-readable summary](../../bench/results/v2/summary.json)
contains per-trial counts, metric deltas, sampled gauges, and separate process resources.

## Method

Gateway, mock backend, and generator ran as three native Windows processes on
one host: Core Ultra 7 155H, roughly 15.5 GiB RAM, Go 1.25.7/windows-amd64.
Each principal combination used three 30-second measurements after a separate
5-second warmup, fresh backend counters/cache, seeded 80% hot / 20% unique keys,
50 ms backend delay, and at most 100 generator requests/connections in flight.
Scheduled mode offered 500 requests/sec. Closed-loop mode maintained up to 100
requests and consequently had a different achieved arrival rate for each mode.
Full mode retained V1's cache policy, coalescing, 32 upstream permits, and 50 ms
permit wait. Proxy mode disabled cache, coalescing, and the limiter.

Percentiles include all sent requests, including errors; table values are medians
of per-trial percentiles, not pooled percentiles. RPS counts successful complete
responses and includes phase drain in elapsed time. No profiles ran concurrently.
Source/binary fingerprints, configuration, process arguments, and raw time series
are retained in each run directory. Runs used uncommitted V2 source above base
commit a567362; do not identify the implementation solely by that base commit.

## Principal steady-state comparison

| Arrival / mode | Successful RPS median [range] | Send p99 ms | Scheduled p99 ms | Client 503 % | Backend calls median | Generator drops |
|---|---|---|---|---|---|---|
| scheduled / proxy | 499.16 [499.16-499.19] | 50.95 | 51.50 | 0.00 | 15,000 | 0 |
| scheduled / full | 499.18 [499.12-499.24] | 55.06 | 57.02 | 0.00 | 2,973 | 0 |
| closed / proxy | 1223.55 [1217.23-1229.09] | 112.35 | 112.66 | 0.00 | 36,808 | 0 |
| closed / full | 5576.64 [5195.16-5601.86] | 106.08 | 106.11 | 10.39 | 18,000 | 0 |

All principal trials had zero transport errors. At the same scheduled arrival
rate, both modes served all 15,000 requests per trial. Full mode reduced backend
calls from 15,000 to 2,969-3,034: approximately 80% less work for this reuse-heavy
workload. It did not increase successful throughput when both already kept up.
Its tail latency was not better in these observations. Scheduled full-mode trials
had 153-424 late arrivals; scheduled proxy had 1-2. A late arrival is processed
more than one interval late, even when it is eventually sent without a drop.
Host activity and fixed run order were not controlled, so these timing differences
cannot be attributed solely to cache or telemetry overhead.

Closed-loop full mode delivered more successful responses for the selected
workload while rejecting about 10.4% of incoming requests. Its sampled active
upstream gauge reached 32, with up to 68 permit waiters. The waiters observed here
are constrained by this generator, not by a newly implemented gateway queue cap.
V2.2 still needs a separate waiter bound and an admission-policy sweep. Absolute
backend-call counts across closed-loop modes do not isolate one optimization:
the modes received different numbers of requests.

One rejected shared operation can produce several client 503s. For example,
closed full trial 1 recorded 19,492 rejected admissions and 19,657 client 503s.
The analyzer checks client status classes separately from upstream admission
operations; treating those two counters as identical would be incorrect.

## Five-minute soak

Full mode, scheduled 500/sec, same workload and 5-second warmup:

- 150,000 offered, sent, and successful measured requests; zero generator drops,
  client 503s, transport errors, or dropped sampled log records.
- 30,031 backend calls; 119,356 immediate cache hits, 30,644 initial misses, and
  613 followers. Misses include followers; backend avoidance is not the hit ratio.
- Send p99 54.95 ms; scheduled p99 56.08 ms. There were 2,541 late arrivals,
  so zero drops does not imply perfectly timed delivery.
- Sampled cache entries never exceeded 1,000; maximum sampled body payload was
  49,981 bytes. This small-payload workload did not stress the 16 MiB body budget.
- After drain, active upstream work, permit waiters, and shared-fetch gauges were
  all zero. Counter deltas and histogram totals reconciled.

| Process | Sample-window CPU seconds | Max sampled RSS MiB | First-quarter median RSS MiB | Last-quarter median RSS MiB |
|---|---|---|---|---|
| gateway | 77.69 | 23.16 | 19.48 | 21.54 |
| mockbackend | 16.61 | 17.41 | 15.96 | 16.79 |
| loadgen | 66.22 | 60.24 | 19.87 | 42.04 |

Gateway RSS increased modestly during the observation window; it was not flat.
This run establishes bounded observed cache cardinality and cleanup, not a proof
of leak-free or long-term memory behavior. The generator intentionally retains
phase samples for exact percentiles, so its memory grows with completed requests.
CPU deltas and RSS maxima come from periodic samples inside the measurement window;
they omit unsampled phase edges and are not exact CPU totals or true peak RSS.

## Provenance and unsuccessful attempts

Canonical steady/soak runs:

- [20261102-191401-088-principal-scheduled-proxy](../../bench/results/v2/20261102-191401-088-principal-scheduled-proxy/manifest.json)
- [20261102-192304-788-verified-scheduled-full](../../bench/results/v2/20261102-192304-788-verified-scheduled-full/manifest.json)
- [20261102-192459-462-verified-closed-proxy](../../bench/results/v2/20261102-192459-462-verified-closed-proxy/manifest.json)
- [20261102-192657-252-verified-closed-full](../../bench/results/v2/20261102-192657-252-verified-closed-full/manifest.json)
- [20261102-192856-737-verified-soak-full](../../bench/results/v2/20261102-192856-737-verified-soak-full/manifest.json)

Earlier baseline attempts incorrectly reported generator failure because of a
Windows process exit-code handling bug; their records are retained but excluded.
A scheduled-full run was interrupted and another stopped on a Windows artifact
sharing error. The latter prompted explicit file sharing and bounded append
retries. These incomplete directories are excluded rather than averaged into
successful trials. Completed reruns above passed reconciliation. Later verified
baseline, generator-drop, and intentional-startup-failure runs are diagnostics,
not additional principal trials; see [verification](verification.md).

V1 historical results remain unchanged. Its short shared-runtime experiments and
these longer three-process runs have different measurement boundaries and cannot
be treated as a direct performance regression/improvement percentage.
