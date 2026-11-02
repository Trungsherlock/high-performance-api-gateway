# Milestone 4 — Local measurements and optimization

Recorded 2026-11-02. These are short, exploratory local experiments, not production
capacity claims. Raw per-trial records and profile summaries are in
[bench/results](../bench/results); reproduction commands are in
[bench/README.md](../bench/README.md).

## Recorded environment and method

- Microsoft Windows 11 Home, version 10.0.26300, native Windows/amd64.
- Intel Core Ultra 7 155H: 16 physical cores, 22 logical processors; about 15.5 GiB
  OS-visible RAM. Go 1.25.7; default GOMAXPROCS (22).
- Normally compiled `cmd/bench` executable; no race/debug instrumentation.
- Gateway, synthetic backend, and closed-loop load generator share one process,
  one Go runtime, and one machine. CPU/RSS below cover all of them.
- 100 workers; backend item delay 50 ms; id 42 body 31 bytes. Unique ids produce
  slightly longer JSON bodies. Caching: 1,000 entries / 16 MiB / 64 KiB per item,
  max TTL 10 seconds. Full mode: 32 permits / 50 ms permit wait.
- Each trial creates fresh servers; steady workloads use a 300 ms warmup and a
  2 second request-start window, followed by draining outstanding requests.
- Three trials per mode and workload. Runs and profiles were performed sequentially.
  Background desktop activity, CPU power state, and thermal conditions were not
  controlled. Short durations and large hot-key ranges limit statistical confidence.
  Mode order was fixed, and before/after optimization trials were not interleaved.
- Hot: primed key 42. Mixed: deterministic 80% key 42 / 20% unique keys. Cold:
  100 requests released behind a start barrier. Expiry: prime, wait 10.1 seconds,
  then release 100 requests. Warmup/priming/waiting calls are excluded from deltas.
- The proxy transport retains its 64-connection per-host ceiling. Direct mode
  does not have the same topology, so it does not isolate proxy overhead.
- The workspace had no Git repository. Source hashes are retained in
  [source-sha256.txt](../bench/results/source-sha256.txt), and machine details in
  [environment.txt](../bench/results/environment.txt).

## Final steady-state results

RPS is successful complete 2xx requests/sec. Percentiles are medians of per-trial
nearest-rank percentiles across **all** responses, including rejections. They are
not pooled percentiles. Error rates aggregate the three trials. CPU is consumed
process CPU time, so it can exceed wall-clock time on a multicore machine. RSS is
an end-of-trial snapshot, not peak usage or retained cache bytes.

| Workload / mode | Successful RPS median [range] | p50 ms | p95 ms | p99 ms | Error % | Backend calls median | CPU seconds | RSS MiB |
|---|---|---|---|---|---|---|---|---|
| Hot / direct | 1963 [1962–1969] | 50.54 | 52.10 | 54.74 | 0 | 4000 | 1.39 | 33.5 |
| Hot / proxy only | 1244 [1243–1249] | 96.28 | 101.45 | 102.71 | 0 | 2596 | 1.44 | 42.7 |
| Hot / cache only | 36035 [34031–50123] | 1.58 | 8.82 | 13.28 | 0 | 0 | 21.38 | 40.2 |
| Hot / full | 28875 [25234–36665] | 2.01 | 10.25 | 15.63 | 0 | 0 | 18.81 | 39.2 |
| Mixed / direct | 1959 [1956–1970] | 50.57 | 52.74 | 55.34 | 0 | 4000 | 1.34 | 34.1 |
| Mixed / proxy only | 1246 [1245–1252] | 96.19 | 100.95 | 102.16 | 0 | 2596 | 1.66 | 42.8 |
| Mixed / cache only | 6224 [6197–6267] | 0.41 | 95.74 | 97.34 | 0 | 2596 | 4.06 | 46.3 |
| Mixed / full | 5894 [5889–5937] | Below resolution | 100.01 | 101.04 | 10.44 | 1312 | 3.78 | 43.1 |

Some very fast Windows measurements were recorded as zero by the clock. They
indicate insufficient effective timing resolution, not zero latency. Actual raw
values are retained. Cache-hit ratio is not instrumented; backend-call avoidance
must not be presented as an exact hit ratio when coalescing is enabled.

### Interpretation

- Warm-cache traffic made zero measured backend calls in every cache/full trial.
  Caching removes the artificial backend delay for that selected workload.
- Proxy-only latency includes queuing behind the 64-connection pool under 100
  workers; its throughput is close to the expected service-limited ceiling.
- Mixed full mode served fewer successful requests/sec than cache-only and shed
  about 10.44% as 503s, while reducing backend calls from a median 2,596 to 1,312.
  The 32-operation limiter protects the backend at an availability cost; it is
  not an across-the-board throughput or latency improvement.
- Full mode's warm-cache median is lower than cache-only in these runs, with
  substantial variation. Do not attribute that difference solely to coalescing:
  the hot path does not need shared fetches, and these are short laptop trials.

## Burst results

Each cold/expiry trial contains 100 requests. All succeeded. Cache-only cold
bursts made 100 backend calls in each trial; full-mode cold and expiry bursts made
one backend call per trial. Coalescing therefore removed 99 of 100 backend calls
in the measured cold-burst comparison. This is a request-count result for that
workload, not a general performance percentage. Raw burst durations and latency
percentiles are retained in `cold-*.jsonl` and `expiry-full.jsonl`.

## Profile-driven changes

### 1. Reuse LRU nodes when replacing an existing key

The replacement microbenchmark uses one key, a 1 KiB body, and one cache-control
header. The allocation-object profile attributed about 15.5% of sampled objects
to `container/list` node creation. Previously replacement removed the old node
and inserted a new one. It now updates the node, moves it to the front, and
adjusts byte accounting, preserving defensive copies and eviction behavior.

| Replacement benchmark | Median ns/op [range], 3 trials | B/op | Allocs/op |
|---|---|---|---|
| Before | 1113 [838.8–1254] | 1568 | 6 |
| After | 732.7 [553.9–842.1] | 1520 | 5 |

Commands and workload were identical: `-bench '^BenchmarkCachePut$' -benchmem
-count 3 -benchtime 1s`. This is about 34% lower median time in this specific
microbenchmark, with substantial timing variation. The deterministic improvement
is one fewer allocation and 48 fewer bytes per replacement. The after profile no
longer attributes replacement allocations to `list.PushFront`. This does not
establish an end-to-end speedup for workloads that rarely overwrite a key.

Evidence: `put-before.txt`, `put-after.txt`, `put-alloc-before.txt`, and
`put-alloc-after.txt` under `bench/results`.

### 2. Reuse reverse-proxy streaming copy buffers

The complete hot-workload allocation profile identified
`httputil.ReverseProxy.copyBuffer` as approximately **72.82% of sampled allocated
bytes**. The proxy allocated a default 32 KiB buffer per response, even for small
cached bodies. A per-proxy `sync.Pool` now implements its standard buffer-pool
interface. Buffers remain independent while in use; the pool can discard them
at garbage collection.

| Warm-key full mode | Median successful RPS [range] | Median p99 ms | Median RSS MiB |
|---|---|---|---|
| Before buffer pool | 16942 [16001–22145] | 17.39 | 35.2 |
| After buffer pool | 28875 [25234–36665] | 15.63 | 39.2 |

Both sets used three trials, 100 workers, 300 ms warmup, a 2s measurement window,
and 50 ms backend delay. The observed median throughput increase was about 70%,
but short sequential trials on a shared laptop do not establish a stable effect
size. All trials had zero errors and zero measured backend calls.

Separate profiled runs used the same 3s workload: the before run recorded 71,014
measured requests, the after run 141,015. Sampled cumulative allocation fell from
roughly 3,314 MB to 1,613 MB across the complete runs, despite more completed work;
the proxy copy buffer disappeared from the dominant allocation sites. These
are cumulative sampled allocations, not live heap or RSS. Profile timing is
excluded from the main table.

Tradeoff: the pool retains buffers for reuse and increased median full-mode RSS
by about 4 MiB in the short hot trials. Its memory is outside the retained-cache
payload budget. CPU profiling still includes substantial Windows runtime/IO and
load-generator work; the experiment does not isolate gateway CPU cost.

Evidence: `before-pool-hot-full.jsonl`, `hot-full.jsonl`, `full-alloc-top.txt`,
`full-alloc-after.txt`, `full-cpu-top.txt`, and `full-cpu-after.txt`.

### Retained simple design and limitations

- Kept defensive response copies: their allocation cost is visible, but removing
  them without an immutable API would weaken isolation and correctness.
- Kept one LRU mutex. This profile does not isolate that mutex as the primary
  bottleneck; runtime lock samples are not evidence that cache sharding is needed.
- The limiter's mixed-workload regression is reported rather than hidden by
  counting rejected requests as successful throughput.
- Initial `hot-before-*` exploratory records overlapped part of a microprofile;
  they are excluded from comparisons. `before-pool-*` contains the subsequent
  separate pre-pool sweep used for the buffer comparison.
- The wrk scripts are provided and shell-syntax checked, but wrk was not available
  in the local Ubuntu environment. These measurements use the native Go runner.
- No open-loop arrival-rate experiment, long-duration soak, separate-machine load
  generator, or gateway-only CPU/RSS measurement was performed.
- Race detection was blocked during these experiments by a local cgo build
  failure. Milestone 5 resolved the compiler-runtime PATH issue and all package
  race tests subsequently passed; see [final verification](verification.md).

## Reproduce

Follow [bench/README.md](../bench/README.md) for build flags, modes, workload
definitions, wrk scripts, profile commands, and artifact interpretation. Run
`./bench/summarize.ps1` to regenerate final-mode summaries. Raw before records
preserve the measured baseline; the current source includes both optimizations.
To reconstruct the earlier implementations, the LRU replacement removed/reinserted
the node, and the reverse proxy had no `BufferPool` configured.

## Template for future experiments

Use the following fields when recording additional experiments.

## Environment

- Date / commit:
- CPU model / physical and logical cores:
- RAM:
- OS / version:
- Native Windows, WSL2, or Linux:
- Go version:
- Gateway, backend, and load generator placement:
- Other significant machine load:

## Workload

- Mode (direct backend / proxy / cache / full gateway):
- Backend artificial delay:
- Response size:
- Key distribution / key count:
- Cache capacity / byte budget / TTL:
- Upstream concurrency limit / permit wait:
- Load generator / version:
- Threads / connections / arrival model:
- Warmup / measured duration / trial count:
- Exact build and run commands:

## Results

| Mode / trial | Successful RPS | p50 | p95 | p99 | Error / 503 % | Backend calls | CPU | RSS |
|---|---|---|---|---|---|---|---|---|
| Pending | | | | | | | | |

## Interpretation

- Hypothesis:
- Profile evidence:
- Change made:
- Before / after under identical workload:
- Variation across trials:
- Tradeoffs and limitations:
