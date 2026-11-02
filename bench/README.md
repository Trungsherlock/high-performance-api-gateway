# Benchmark reproduction

Run one experiment at a time on an otherwise idle machine. Retain every trial,
including failures. These are closed-loop experiments: workers wait for responses
before issuing their next request; concurrency is not a fixed arrival rate.

## Native Go load runner

```powershell
./bench/run.ps1 -Scenario hot -Duration 2s -Trials 3
./bench/run.ps1 -Scenario mixed -Duration 2s -Trials 3
```

The runner compiles normally, without race instrumentation. Every independent
trial starts a new mock backend and gateway on ephemeral loopback ports. Both
servers and the load generator share one process/runtime; CPU and RSS therefore
describe that entire process, not the gateway alone. Repetition within one process
can retain runtime allocations, so RSS is an end-of-trial snapshot, not a peak.
Windows process counters are implemented; CPU/RSS fields are unavailable (zero)
on other platforms. Never interpret unavailable counters as zero resource use.

Default parameters: 100 workers, 50 ms backend delay, 300 ms unmeasured warmup,
three measured trials. Hot mode primes key 42 before warmup. Mixed mode is a
deterministic 80% hot / 20% unique-key sequence, retained across warmup and the
measurement. All requests use an ordinary client with compression disabled so
the project's strict cache policy permits them. The response for id 42 is 31 bytes.

Modes:

| Mode | Cache | Coalescing | Work limiter |
|---|---|---|---|
| direct | No gateway | No | No |
| proxy | Off | Off | Off |
| cache | On | Off | Off |
| full | On | On | 32 permits / 50 ms wait |

The shared proxy transport retains its 64-connection per-host ceiling in all
gateway modes. This can queue proxy-only requests even with the explicit limiter
off. The direct backend mode has a different connection topology and is context,
not an isolated measure of proxy overhead.

JSONL records include successful RPS, nearest-rank p50/p95/p99 in milliseconds,
request/error/503 counts, measured backend-call deltas, process CPU seconds,
end-of-trial RSS, and environment metadata. Latency percentiles include failed
requests; successful RPS counts only complete 2xx responses. Measurement includes
draining requests started before the duration boundary. Cache-hit ratio is not
instrumented: backend-call avoidance includes coalescing and is not a hit ratio.

Some sub-millisecond measurements on the local Windows clock are zero. Treat
these as below the effective measurement resolution, not zero response time.
Summarize the canonical final JSONL files with `./bench/summarize.ps1`; it reports
medians of per-trial percentiles, not percentiles pooled across all trials.
Files prefixed `before-pool-` retain the measurements before buffer reuse.

Cold and expiry scenarios release one request per worker with a shared start
barrier. They are burst checks, not steady-state throughput measurements:

```sh
go build -o bin/bench ./cmd/bench
./bin/bench -mode full -scenario cold -trials 3
./bin/bench -mode full -scenario expiry -trials 3
```

Expiry primes a key then waits 10.1 seconds per trial. Scheduling and connection
establishment mean a start barrier does not guarantee every request reaches the
backend at the same instant. Deterministic unit/integration tests remain the
correctness authority for concurrency, overload, and recovery.

## wrk on Linux/WSL2

Requires Go and wrk installed inside that environment. Start compiled backend and
gateway binaries in separate terminals; choose the desired mode explicitly.

```sh
go build -o bin/mockbackend ./cmd/mockbackend
go build -o bin/gateway ./cmd/gateway
./bin/mockbackend -delay 50ms
# Another terminal: proxy-only baseline
CACHE_ENABLED=false ./bin/gateway -coalesce=false -max-concurrent=0
# Or cache-only:
CACHE_ENABLED=true ./bin/gateway -coalesce=false -max-concurrent=0
# Or full:
CACHE_ENABLED=true ./bin/gateway -coalesce=true -max-concurrent=32 -permit-wait=50ms
# Another terminal:
bash bench/run.sh hot-key
bash bench/run.sh mixed
```

The script performs a 3s warmup then three 30s trials with four threads and 100
connections. Mixed scripts use distinct per-worker key sequences. wrk's aggregate
RPS includes HTTP errors: retain its non-2xx/3xx count and socket errors, inspect
backend `/stats` deltas separately, and do not label total RPS successful RPS.
The scripts are supplied for reproduction; the recorded local measurements use
the native Go runner, since wrk and Go were absent from the available Ubuntu WSL.

## Microbenchmarks and profiles

```sh
go test ./internal/cache ./internal/coalesce -run '^$' -bench . -benchmem -count 3
go test ./internal/cache -run '^$' -bench '^BenchmarkCachePut$' -benchtime 2s -memprofile bench/profiles/put.heap
go tool pprof -top -alloc_objects bench/profiles/put.heap
```

Create `bench/profiles` first. Profiles are ignored; textual evidence and JSONL
records are retained under `bench/results`. `BenchmarkGroupJoin` holds one flight
open and measures registration, while `BenchmarkGroupLeader` includes launching
and waiting for a new operation. Neither substitutes for network benchmarks.

For CPU and heap/allocation evidence from the complete hot workload:

```sh
go build -o bin/bench ./cmd/bench
./bin/bench -mode full -scenario hot -duration 3s -trials 1 -cpuprofile bench/profiles/full.cpu -memprofile bench/profiles/full.heap
go tool pprof -top bench/profiles/full.cpu
go tool pprof -top -alloc_space bench/profiles/full.heap
```

Profiles perturb timing. Profile runs are recorded separately and excluded from
the main performance comparison. No profiling HTTP endpoint is exposed.
