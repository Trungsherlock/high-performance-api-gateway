# V2.0–V2.1: isolated processes and trustworthy measurements

## Run the experiment

From the repository root in Windows PowerShell with Go available:

```powershell
# Preserved V1 servers, current external generator; no new instrumentation.
./bench/run-v2.ps1 -Baseline -Mode proxy -Seconds 2 -Warmup 1 -Trials 1 -Label baseline-proxy
./bench/run-v2.ps1 -Baseline -Mode full -Seconds 2 -Warmup 1 -Trials 1 -Label baseline-full

# Principal matrix: each combination uses three fresh server pairs.
foreach ($arrival in @('scheduled','closed')) {
  foreach ($mode in @('proxy','full')) {
    ./bench/run-v2.ps1 -Observe -Mode $mode -Arrival $arrival -Seconds 30 -Warmup 5 -Trials 3 -Label "principal-$arrival-$mode"
  }
}
./bench/run-v2.ps1 -Observe -Mode full -Seconds 300 -Warmup 5 -Trials 1 -Label soak-full

# Analyze one completed run; assertions detect accounting inconsistencies.
python bench/summarize-v2.py bench/results/v2/<run-directory>
```

The default workload is seeded 80% hot-key / 20% unique-key traffic, 50 ms backend
service delay, 500 scheduled requests/sec, and at most 100 generator requests in
flight. `-Workload hot`, `-Rate`, `-Concurrency`, `-Seed`, and `-DelayMS` are explicit
controls. Closed-loop mode sends as slots become available; its offered rate is
an outcome rather than a fixed input. No Redis is required.

The harness builds three binaries, chooses loopback ports, starts the backend and
gateway, waits for HTTP readiness, then starts the generator as a third process.
It samples CPU and RSS per process approximately once per second and captures
server logs. Every trial starts new backend counters and an empty gateway cache.
Artifact appends use explicit file sharing and bounded retries for transient
Windows sharing errors; failed writes still fail the run rather than silently
losing telemetry. Completion markers distinguish finished runs from interruptions.
A `finally` block stops all children and restores `CACHE_ENABLED` on normal
completion or errors. A watchdog bounds generator runtime. Forced termination of
the PowerShell host or machine cannot execute cleanup. Port discovery releases
its temporary listener before server bind; a competing bind causes an observable
startup failure with cleanup, rather than guaranteeing race-free reservation.

V1 is tagged `v1-baseline` at `a567362` (which includes the planning documents);
the earlier implementation commit remains `465d9f4`. Development stays on the
existing `v2-multi-instance-caching-and-overload-experiments` branch. Baseline mode
uses a detached source worktree under ignored `bin/v1-source` to build unchanged
servers. Historical V1 results are untouched.

Each timestamped directory under `bench/results/v2/` contains a manifest, source
fingerprints, per-trial process arguments/PIDs, generator phase records, backend
counters, resource samples, and, with `-Observe`, periodic and final metrics.
Uncommitted source is identified by fingerprints as well as the commit and diff;
results must not be attributed solely to the base commit.

## Arrival and latency accounting

Scheduled mode computes every due time from a monotonic start and a fixed rate.
It does not wait for a previous request to finish before scheduling the next.
Every scheduled arrival is counted: a full generator slot set or an arrival
processed after the phase's end becomes a **generator drop**. Overdue arrivals
within the phase can be sent immediately, so scheduling lag must be inspected.
`LateArrivals` counts arrivals processed more than one arrival interval late.
There are no application-level retry loops or automatic response decompression.
The standard HTTP client retains its redirect behavior; the mock workload emits
no redirects.

Each phase records offered, sent, completed, successful, rejected (503), transport
errors, status counts, drops, and late arrivals. The connection limit equals the
in-flight limit, and the default whole-request deadline is three seconds.
Percentiles include failed sent requests and use the nearest-rank rule:

- Send latency: immediately before initiating the HTTP request through complete
  body consumption or failure; includes connection acquisition.
- Scheduled latency: scheduled due time through that same completion.
- Scheduling lag: actual initiation minus scheduled due time.

Unsent drops have no response latency; their counts accompany the percentiles.
A deterministic seed controls key selection, not thread scheduling or exact hit
ratios. Warmup is a separate record and drains before measurement starts. Before
and after snapshots give exact phase backend/metric deltas; do not use cumulative
final counters as measurement-only counters. Successful RPS uses elapsed phase
wall time including drain, while scheduled arrivals are offered over the configured
request-start window. Very fast native Windows timings may be below resolution.
The generator retains samples for exact percentiles, so its memory can grow with
completed requests during a phase; it is separate from gateway memory.

## Administration and telemetry

Gateway flags:

```text
-admin-addr 127.0.0.1:8081  Dedicated numeric loopback listener; empty disables it
-instance gateway         Instance identifier, at most 64 bytes
-request-log-every 1000    One sampled log per N requests; 0 disables request logs
```

The admin listener exposes JSON `/metrics`, liveness `/healthz`, and `/readyz`.
Readiness means local initialization is complete. It does not probe backend health
and there is no optional cache dependency yet. Startup/bind failure exits instead
of pretending readiness. The public `/healthz` stays available; admin and health
requests are excluded from data-request metrics. Metrics are not exposed through
the public listener.

Metrics use fixed names without URL, key, request ID, credential, or tenant labels:

- Counters: incoming/completed handlers, final status classes, aborted handlers,
  cache hits/misses/bypasses, local followers, backend attempts/errors, permit
  rejections, dependency errors, and dropped log records.
- Gauges: active outbound operations through body completion, permit waiters,
  active shared-fetch functions, allocated cache entries, and body payload bytes.
- Histograms: request lifetime, upstream body lifetime, permit acquisition wait,
  and reserved cache-dependency latency. Buckets are **non-cumulative**, with
  upper bounds in seconds and a final overflow bucket; count and sum are supplied.

A cache miss counts once per eligible incoming request, including followers;
internal leader rechecks do not inflate hits/misses. Followers are a subset of
misses. Cache bypass is measured when the cache is enabled; disabled-cache runs
leave cache counters zero. Backend attempts are measured inside the limiter, so
permit rejections do not count as attempts. One rejected shared flight can
produce multiple client 503 responses, so permit rejections count failed admission
operations rather than rejected clients. Client status counts remain separate. Upstream status errors are HTTP
responses, separate from transport/body errors. A handler that aborts after
sending 200 can increment both status_2xx and aborted; a successful generator
result requires its body to complete. Dependency counters/histograms remain zero
until an actual dependency is introduced in a later milestone. Allocated expired
cache entries remain included until lazy removal.

Structured JSON request logs include a locally generated instance/request ID,
a fixed route class, response/abort outcome, status, and elapsed time. They omit
URLs, query values, headers, and bodies. A 256-record nonblocking queue bounds
pending log records; full queues drop logs and increment a counter. Sampling and
queueing bound per-request logging work, not total log-file size. The process owns
the log worker; process exit can discard queued records. Normal server startup
messages remain plain text, so log files contain both startup text and JSON rows.

## Measurement limits

CPU/RSS belong to distinct native processes on the same host; CPU, network,
thermal state, and OS scheduling still share that host. Resource summaries use
samples inside the measurement timestamps, excluding warmup where sampled.
CPU deltas omit unsampled phase edges; maximum sampled RSS is not true peak RSS.
Profile runs are separate from these experiments. A five-minute soak is evidence
about this workload, not a proof of long-term stability or production readiness.
See [results](results.md) for collected observations.


## V2.2 update

Current CLI admission defaults are 32 active upstream operations, 32 queued
permit waiters, and a 50 ms wait budget. Use `-max-waiters -1` to reproduce the
legacy waiter policy in the V2.0/V2.1 results. The harness accepts `-MaxWaiters`,
`-MaxConcurrent`, and `-PermitWaitMS`, plus `-Workload unique` and timed slowdown
flags. See [V2.2 policy, experiments, and defaults](admission.md) for commands,
rejection reasons, cancellation semantics, and the remaining unbounded resources.


## V2.3 update

Independent gateway instances and a health-aware round-robin front are implemented.
Use `./bench/run-v23-study.ps1` for controlled cold bursts, stop/restart, and
`1x32` versus `2x16` permit comparisons. See [the topology guide](multi-instance.md)
for per-instance readiness, separate front CPU/RSS, generator drops, probe failures,
and explicit retry/coordination limits. No Redis dependency is introduced yet.

## V2.4 shared response cache

Use `python bench/run-v24.py --redis-addr 127.0.0.1:6379` with an isolated Redis
and freshly built gateway/backend binaries for the three-mode comparison.
See [V2.4 setup, acceptance checks, and collected results](shared-cache.md).

## V2.5 bounded fill leases

Use `python bench/run-v25.py --redis-addr 127.0.0.1:6379` with an isolated Redis
and fresh gateway/backend/burst binaries. It compares shared and leased bursts
and kills an owner during a fill. See [lease setup, semantics and results](leases.md).

## V2.6 fault injection and degraded modes

`python bench/run-v26.py` owns an isolated Redis and tests outage, delay, OOM,
restart and corrupt data with both policies. `--race` selects a prebuilt
race-instrumented gateway. See [setup, policy semantics and results](faults.md).

## V2.7 final comparison

`python bench/run-v27.py --complete` builds, compares four configurations, runs
cold bursts and a five-minute soak, verifies ordinary/race tests and shutdown,
reproduces faults, and rebuilds a source-only smoke snapshot. `--quick --complete`
is a functional check. `--resume <directory> --skip-build --complete` preserves
completed trials when settings/application hashes match. See [final methods,
results, retained failures and process resources](final.md).
The final 25-trial study is `20261103-224701-v27`; its SUCCESS follows a recorded
startup interruption, with original FAILED/logs and resume-1.json preserved.
