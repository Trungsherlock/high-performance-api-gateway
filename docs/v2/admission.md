# V2.2 - Bounded admission and overload experiments

Implemented and measured 2026-11-02 (America/Chicago). V1 and V2.0/V2.1 historical
results are unchanged. This milestone bounds queued upstream operations, not all
requests or process memory.

## Policy and ownership

The gateway CLI defaults to 32 active upstream operations, at most 32 queued
permit waiters, and a 50 ms permit wait:

```powershell
go run ./cmd/gateway -max-concurrent 32 -max-waiters 32 -permit-wait 50ms
```

A request first tries an immediately available permit. Otherwise it must reserve
one queue slot before waiting. A full queue returns 503 immediately. Acquisition,
cancellation, or timeout releases the queue slot; an acquired upstream permit
remains held through body EOF/error/close. Cancellation checked after acquisition
also releases that permit before starting backend work. Cache hits bypass both
permit and queue admission. Coalesced followers share the leader's operation and
do not acquire their own upstream queue slots.

- `-max-waiters 0`: no queued waiting; saturation returns `queue_full`.
- `-max-waiters -1`: original unbounded-waiter comparison policy; the wait deadline
  still applies. The library's existing `limiter.New` and nil gateway
  `Options.MaxWaiters` preserve this behavior for older benchmark callers.
- `-max-concurrent 0`: disables upstream admission limiting entirely.

503 responses carry `X-Gateway-Rejection: queue_full` or `wait_timeout`. The fixed
metrics `permit_queue_full` and `permit_wait_timeout` sum to `permit_rejections`;
sampled structured logs include the same bounded reason. Shared failure fanout
can still produce more client 503s than rejected upstream admission operations.
If wait duration is zero, an occupied queue is still classified queue-full first;
an admitted waiter immediately receives wait-timeout.

This semaphore-based policy does not promise FIFO fairness: new arrivals may
acquire released permits before older waiters. Wait deadlines bound how long an
operation can remain queued. Active operations and queue slots are separate
resources, and cancellation does not release somebody else's permit.

## Reproduce

```powershell
# Build once, then run screening and repeated separate-process cases.
./bench/run-v22.ps1
python bench/summarize-v22.py bench/results/v2/<v22-study-index>.jsonl

# One shorter individual case:
./bench/run-v2.ps1 -Observe -Workload unique -Seconds 6 -Warmup 1 -Trials 3 -Rate 1200 -MaxConcurrent 32 -MaxWaiters 32 -PermitWaitMS 50
```

`run-v2.ps1` additionally accepts `-SlowAfterSeconds`, `-SlowForSeconds`, and
`-SlowDelayMS`. For example, a backend can switch from 50 ms to 200 ms at process
uptime 4 seconds, remain slow for 3 seconds, then recover. Each request retains
its selected delay. Backend `/stats` now includes active/peak work, current delay,
and process uptime; the harness captures those fields approximately every second.
`-SkipBuild` deliberately reuses existing binaries and should only follow a build
of the intended source. Each run records binary hashes as well as source hashes.

The study uses unique eligible GET keys so every request misses the cache. This
isolates admission from hot-key caching/coalescing; cache-hit bypass is verified
separately by an integration test. Generator concurrency/connection limit is 512,
arrival mode is scheduled, request deadline is three seconds, and warmup is one
second. Fifteen exploratory cases run four seconds each. Eight comparison cases
run three trials each: six-second healthy/overloaded/slow measurements and
nine-second recovery measurements. These are focused short experiments, not a
new production-capacity or long-soak claim. No profiling ran alongside them.

The backend's configured capacity budget matters: increasing permits changes how
much work the backend must handle, not just the gateway's efficiency. All cases
run three native processes on the same Windows host with Go 1.25.7; source base
is commit 1bbc4d7 with the recorded uncommitted V2.2 changes. Fixed case order,
host activity, and short durations limit causal and statistical conclusions.

## Exploratory screening

Except where listed, settings are 32 permits, 32 waiters, 50 ms wait, 1,200
arrivals/sec, and 50 ms backend delay. p95/p99 include all sent responses,
including rejections. RPS counts successful complete bodies and includes drain.

| Case | Permits | Queue | Wait ms | Arrival/sec | Backend ms | Success RPS | Client 503 % | p95 ms | p99 ms | Sampled queued peak |
|---|---|---|---|---|---|---|---|---|---|---|
| screen-c8 | 8 | 32 | 50 | 1200 | 50 | 158.2 | 86.50 | 99.64 | 100.38 | 32 |
| screen-c16 | 16 | 32 | 50 | 1200 | 50 | 314.1 | 73.19 | 99.70 | 100.48 | 32 |
| screen-c32 | 32 | 32 | 50 | 1200 | 50 | 628.4 | 46.35 | 99.69 | 100.31 | 32 |
| screen-c64 | 64 | 32 | 50 | 1200 | 50 | 1184.8 | 0.00 | 50.72 | 51.03 | 0 |
| screen-q0 | 32 | 0 | 50 | 1200 | 50 | 619.0 | 47.75 | 50.73 | 51.11 | 0 |
| screen-q8 | 32 | 8 | 50 | 1200 | 50 | 628.0 | 46.92 | 77.35 | 79.49 | 8 |
| screen-q64 | 32 | 64 | 50 | 1200 | 50 | 626.5 | 46.48 | 100.00 | 100.60 | 60 |
| screen-q-1 | 32 | -1 | 50 | 1200 | 50 | 628.6 | 46.35 | 99.91 | 100.41 | 60 |
| screen-w0 | 32 | 32 | 0 | 1200 | 50 | 621.1 | 47.58 | 50.68 | 50.99 | 0 |
| screen-w10 | 32 | 32 | 10 | 1200 | 50 | 629.9 | 46.73 | 60.06 | 60.49 | 13 |
| screen-w100 | 32 | 32 | 100 | 1200 | 50 | 627.5 | 46.46 | 100.11 | 100.89 | 32 |
| screen-r400 | 32 | 32 | 50 | 400 | 50 | 395.1 | 0.00 | 50.88 | 51.21 | 0 |
| screen-r800 | 32 | 32 | 50 | 800 | 50 | 626.7 | 19.72 | 99.71 | 100.28 | 32 |
| screen-r1600 | 32 | 32 | 50 | 1600 | 50 | 628.6 | 59.72 | 99.76 | 100.36 | 32 |
| screen-slow | 32 | 32 | 50 | 800 | 200 | 157.0 | 79.19 | 245.84 | 248.58 | 32 |

A zero-length queue lowered tails but rejected slightly more requests in the
screen. A larger queue of 64 or legacy unlimited waiting did not raise successful
throughput materially relative to 32, while retaining more waiters. A 10 ms wait
had lower tail latency with slightly more rejections than 50 ms; 100 ms added no
clear capacity benefit with the 32-slot queue. These alternatives are tradeoffs,
not proof of one universally optimal setting. Sixty-four permits accepted this
healthy offered load but allowed approximately twice the backend work budget.

## Repeated comparisons

Values are median successful RPS with trial range, median per-trial p95/p99, and
aggregate client rejection percentage. Queue peaks and RSS are maxima across
periodic samples. Recovery rows combine the entire slowdown/recovery interval.

| Case | Success RPS median [range] | 503 % | p95 ms | p99 ms | Sampled queued peak | Backend peak | Max sampled gateway RSS MiB |
|---|---|---|---|---|---|---|---|
| healthy-q32 | 396.8 [396.8-396.8] | 0.00 | 50.89 | 51.24 | 0 | 21 | 19.47 |
| overload-q32 | 631.3 [631.3-631.4] | 46.68 | 99.73 | 100.32 | 32 | 32 | 26.54 |
| slow-q32 | 157.1 [156.9-157.3] | 79.57 | 245.94 | 248.16 | 32 | 32 | 24.23 |
| recovery-q32 | 473.7 [473.5-474.1] | 40.15 | 242.66 | 246.98 | 32 | 32 | 26.91 |
| healthy-q-1 | 396.7 [396.7-396.8] | 0.00 | 51.03 | 51.50 | 0 | 27 | 19.52 |
| overload-q-1 | 631.3 [627.3-631.3] | 46.77 | 99.99 | 100.74 | 61 | 32 | 28.95 |
| slow-q-1 | 157.8 [157.7-157.9] | 79.47 | 249.25 | 250.14 | 41 | 32 | 24.27 |
| recovery-q-1 | 474.3 [474.1-474.4] | 40.07 | 247.12 | 249.80 | 41 | 32 | 25.68 |

Healthy comparison is 400/sec against a 50 ms backend; overloaded comparison is
1,200/sec against 50 ms; slow comparison is 800/sec against 200 ms. Both policies
served the healthy case without rejections. Under healthy overload, each delivered
about 631 successful requests/sec, while the 32-slot cap held observed queueing to
32 versus up to 61 legacy waiters. Slow overload remains capacity-limited and
rejects most incoming demand; queueing cannot create backend capacity.

RSS does not isolate memory per waiter: goroutines, flight records, transports,
cache contents, allocation rate, and GC retention all contribute. The capped
policy's RSS was sometimes higher in recovery. The report therefore records
memory observations without claiming a fixed byte saving or a total RSS bound.
Sampled maxima are not true continuous maxima; deterministic tests establish
queue bounds independently of sampling.

## Recovery behavior

Slowdown is keyed to backend uptime, not generator start. The phase records its
starting backend uptime, and backend sample files record the actual delay window.
Three-second generator cohorts are grouped by scheduled start; the first two
cohorts straddle injection boundaries. The final cohort is fully after recovery.
At 800 arrivals/sec, a healthy 32-permit backend is still overloaded, so successful
recovery means returning toward roughly 640 backend completions/sec, not zero 503s.

| Policy | Final cohort successful requests/sec median | Final cohort p99 ms median | Final work gauges |
|---|---|---|---|
| recovery-q32 | 634.0 | 100.47 | all zero |
| recovery-q-1 | 634.7 | 100.53 | all zero |

Both policies returned to the healthy throughput range and drained active work,
permit waiters, and flights after completion. No trial had generator drops or
transport errors. Backend observed peaks never exceeded the configured permit
limit, and sampled capped queues never exceeded their configured capacities.
Reason counters, client statuses, backend calls, histogram totals, and phase
counts reconciled in every trial.

## Default decision and limits

Keep the existing 32-operation backend budget and 50 ms wait budget, and add a
32-waiter CLI default. The evidence supports a finite cap without a meaningful
throughput loss versus legacy waiting in the repeated cases. Thirty-two absorbs
more waiting than zero/eight in the screening while avoiding larger queues that
showed no meaningful capacity gain. Preserving the existing wait budget favors
burst absorption over the lower latency of 10 ms; it is an explicit conservative
policy choice, not a claim that 50 ms is optimal. Revisit these values for a real
backend capacity budget, workload, and latency/availability objective.

This does not cap inbound connections, shared-flight followers, distinct-flight
registration, or all gateway memory. Shared operations keep independent lifetimes;
canceling one follower does not cancel a fetch still useful to other callers.
V2.3 and later multi-instance/shared-cache work is not part of this milestone.

## Evidence and verification

- [Study index](../../bench/results/v2/v22-study-20261102-195158.jsonl)
- [Per-trial summary](../../bench/results/v2/v22-study-20261102-195158.summary.json)
- [Ordinary test output](../../bench/results/v2/v22-tests.txt)
- [Race test output](../../bench/results/v2/v22-race.txt)

Tests cover a 200-request burst with exactly eight admitted waiters and 192
queue-full rejections, cancellation cleanup without releasing active work,
zero-queue and timeout reasons, successful reuse after cleanup, cache hits during
saturation, and backend slowdown/recovery selection. Ordinary tests, race tests,
vet, build, formatting, and whitespace checks passed. The harness left no server
or generator processes after the study. The initial external smoke attempt found
a backend-argument initialization mistake in the harness edit; it was corrected
and a successful smoke rerun is retained separately from the study.
