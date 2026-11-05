# V2.6: dependency failures and degraded modes

V2.6 makes the availability/backend-protection choice configurable, adds separate
cache admission limits, and exercises Redis faults with two gateways. Leases
remain best effort; none of these policies provide exactly-once backend work.

## Policy choice

Use `-leases -cache-mode layered` (or `shared`) with `-degradation`:

| Event | `protection-first` (default) | `available-local` |
|---|---|---|
| Fresh L1 response | serve without Redis | serve without Redis |
| Shared read/acquire failure before fill | 503 coordination_error | local coalesced fill under local limiter |
| Coordination wait expires | 503 coordination_timeout | local coalesced fill under local limiter |
| Explicit ownership rejection | 503 lease_lost, no L1 | same |
| Ambiguous publication error | 503, no L1 | deliver completed safe response to current local flight, no L1 |
| Backend timeout/error | existing 504/502; release resources | same; do not issue another backend fetch |

Available-local fallback after a pre-fill failure stores an eligible response in
L1 only. It never uses the unconditional shared writer. Fallback uses the remaining
overall shared-work deadline; it does not restart the request budget. Two instances can fill the
same key and retain different snapshots until their original TTLs expire. Local
permits remain per instance: 2 x 16 can admit a summed 32 active backend operations;
there is no globally strict limiter during healthy or degraded operation.

Ambiguous publication may have committed in Redis despite a missing reply. This
mode can return the completed public response to its current local callers, but
never warms L1 from an unconfirmed result and never refetches because publication
failed. A later request may safely discover the committed value via a shared read.
If ownership was explicitly rejected, both modes discard that result. An expired
fill deadline also prevents ambiguous-result delivery. Deferred token-checked
cleanup has its own bounded operation; process death still relies on lease expiry.

The policy flag affects lease-dependent paths. Nonlease V2.4 modes retain their
existing available-local behavior. Readiness still means local initialization;
fresh L1 availability does not require Redis readiness. No automatic backend or
Redis command retry loop is added. Redis restart can produce a first-operation
failure on an old pooled connection before a subsequent operation reconnects.

## Separate resource limits

The gateway CLI now defaults to:

- `-max-cache-flights 64`: at most 64 registered distinct local flight keys.
  Registration checks the limit synchronously under the group mutex, before
  creating work. Existing keys can join while the distinct-key limit is full.
- `-max-cache-waiters 1024`: at most 1024 cache-miss clients inside the coalesced
  transport path, including leaders. This is a per-instance aggregate, not 1024
  followers per key. It covers waiting for the result/response headers, not a
  cached response's entire transmission to a slow client.
- Full admission rejects immediately with 503 cache_flights_full or
  cache_waiters_full. L1 hits bypass both limits. Cancellation releases the
  client's slot while bounded shared work may continue; the flight slot remains
  occupied until that work finishes. No unbounded registration queue is added.

These limits apply when caching and local coalescing are enabled, including
local-only mode. `CACHE_ENABLED=false` bypasses caching; `-coalesce=false` has no
shared flight queue. Library callers can set Options.CacheAdmission; nil preserves
legacy API behavior. Upstream concurrency/permit queue limits remain separate.
Inbound connections, slow cached-response consumers and all retained process
memory are not bounded by these cache admission limits. This is not a total-RSS
or denial-of-service protection claim.

Tests hold backend work to demonstrate full queues, same-key joining, immediate
rejection, L1 bypass, client cancellation without lost flight bounds, and capacity
reuse after completion. The new cache_waiters gauge tracks admitted cache-miss
clients; cache_waiter_rejections and cache_flight_rejections identify shedding.

## Visibility

- degraded_flights and fallback_dependency_error / fallback_coordination_timeout /
  fallback_publication_error count activations per local flight, not successful
  clients. Existing dependency_errors and read/write/lease error counters persist.
- Fallback responses carry an allowlisted X-Gateway-Degraded reason. Later L1 hits
  have no such marker: they did not perform a dependency fallback on that request.
- Sampled request logs include degraded_reason and bounded rejection_reason.
  A nonblocking cache_fallback event records activation even when a subsequent
  backend fetch fails. The existing 256-entry log queue drops rather than blocking;
  drops remain counted. `-request-log-every 0` disables this log worker/events.
- No URL, cache key, lease token, body, auth value or unbounded reason becomes a
  metric label or fallback-event field. Log tests verify reason filtering and
  exclusion of request secrets.

## Reproduce the fault matrix

`bench/run-v26.py` owns an isolated Redis server, backend and two gateway processes.
It stops its children on completion/error. It never targets an existing Redis or
flushes someone else's database. Redis must be **7.4.2**, with 64 MiB noeviction and
persistence disabled. The harness writes its own LF config and allocates ports.

On Windows with the previously built WSL Redis binary:

```powershell
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/burst.exe ./cmd/burst
python bench/run-v26.py --wsl Ubuntu --redis-server bin/redis-build/redis-server
```

For the race-instrumented matrix used in the recorded run:

```powershell
$env:Path = 'C:\msys64\mingw64\bin;' + $env:Path
go build -race -o bin/gateway-race.exe ./cmd/gateway
python bench/run-v26.py --race
```

The harness sets GORACE=halt_on_error=1. `--trials N` repeats each case; default one
is a deterministic fault check, not repeated statistical performance evidence.
On Linux build extensionless binaries and supply `--redis-server /path/to/redis-server`.

For a fresh checkout without Redis, build the pinned official source in a Linux
or WSL shell. Run from the repository directory; compiling under native /tmp
avoids slow Windows-mounted compilation:

```sh
repo_dir="$PWD"
build_dir=$(mktemp -d /tmp/gateway-redis-XXXXXX)
curl -fsSLo "$build_dir/redis.tar.gz" https://download.redis.io/releases/redis-7.4.2.tar.gz
printf '%s  %s\n' 4ddebbf09061cbb589011786febdb34f29767dd7f89dbe712d2b68e808af6a1f "$build_dir/redis.tar.gz" | sha256sum -c -
tar -xzf "$build_dir/redis.tar.gz" -C "$build_dir"
make -C "$build_dir/redis-7.4.2" -j4 MALLOC=libc OPT=-O1 REDIS_CFLAGS= REDIS_LDFLAGS=
mkdir -p "$repo_dir/bin/redis-build"
cp "$build_dir/redis-7.4.2/src/redis-server" "$repo_dir/bin/redis-build/redis-server"
```

Full Go tests use a separate isolated Redis pointed to by REDIS_TEST_ADDR; they do
not shut down/reconfigure that server. Start one in a WSL/Linux terminal:

```sh
./bin/redis-build/redis-server --bind 127.0.0.1 --port 16379 --save "" --appendonly no --maxmemory 64mb --maxmemory-policy noeviction
```

```powershell
$env:REDIS_TEST_ADDR = '127.0.0.1:16379'
go test ./... -count=1 -timeout 60s
go test -race ./... -count=1 -timeout 60s
go vet ./...
go build ./...
python bench/run-v25.py --redis-addr 127.0.0.1:16379 --trials 1
```

Without REDIS_TEST_ADDR, real-server tests explicitly skip. The V2.5 harness
regression rechecks killed-owner progress on the current gateway implementation.

## Collected results

[Ten-case race matrix](../../bench/results/v2/20261103-223343-v26/trials.jsonl),
[summary](../../bench/results/v2/20261103-223343-v26/summary.json),
[manifest and build flags](../../bench/results/v2/20261103-223343-v26/manifest.json),
[verification](../../bench/results/v2/20261103-223343-v26/verification.json).
The actual gateway build has -race=true. Runtime source hashes match the tested
code, and process logs contain no race reports. Redis ran in WSL2; Go servers and
the burst generator ran as separate native Windows processes on the same host.

Each topology warmed one public key on both instances with a single shared fill.
The fault cases used a 50ms backend, 50ms dependency deadline, 1s leases, 600ms
owner deadline, 500ms coordination wait and 10-20ms polling. Each instance had
16 upstream permits/queue slots, 16 cache flights and 128 cache-miss client slots.

| Fault: 100 cold requests, 50 per gateway | Protection-first | Available-local |
|---|---|---|
| Redis stopped | 100 x 503, 0 backend calls | 100 x 200, 2 backend calls |
| Redis paused 350ms | 100 x 503, 0 backend calls | 100 x 200, 2 backend calls |
| Redis OOM/noeviction | 100 x 503, 0 backend calls | 100 x 200, 2 backend calls |

Fresh L1 requests succeeded during every fault. Available-local created two local
fallback flights per cold burst; its observed backend peak was two, versus one
for the protection-first scenarios. The workload does not exercise all 32 aggregate
permits, so this is duplicate-work evidence, not a capacity measurement. The
outage burst marked 71 responses as degraded and served 29 later clients directly
from L1; all 100 succeeded. Flight/error counters are not equated with client counts.

The memory fixture first filled bounded 32 KiB values until an actual OOM at a
2 MiB limit, then set maxmemory to one byte to remove allocator-headroom ambiguity
for the admission check. This is deterministic capacity rejection, not a realistic
memory-pressure benchmark. It restored 64 MiB and deleted only its pressure keys
before testing recovery. Corrupt JSON was treated as a miss and replaced by one
valid fill, with no dependency errors, in both modes.

Redis restart preserved fresh L1 responses. The harness also killed/restarted one
gateway, verified empty L1/counters, and fetched again. Protection-first retained
a first recovery probe of 503 on the survivor's stale Redis connection, followed
by 200; available-local returned 200 through fallback and made a second backend
fill. Both subsequently reused a newly filled shared key successfully. Every
external recovery probe is retained in the raw result, rather than silently retried.
All cases finished with drained gateway/backend work and no surviving lease keys.

[Current-code killed-owner regression](../../bench/results/v2/20261103-223608-v25/summary.json)
passed: shared-only burst two attempts, leased burst one, killed-owner survivor
success after expiry with two total attempts (one abandoned). The interrupted
owner's client transport error remains recorded. Real Redis tests also delay an
old owner past expiry and prove it cannot publish an older response version or
delete a newer lease. This simulates a delayed owner; no OS process-pause claim is made.

Full ordinary/race suites, vet and build passed, including real Redis stale-owner,
expiry, rejected-L1, ambiguous-commit, backend-timeout and permit-reuse cases.
Logs: [ordinary](../../bench/results/v2/v26-tests.txt),
[race](../../bench/results/v2/v26-race.txt). Short fault tests do not establish
production reliability, long-term RSS stability or zero-error restart behavior.

Two diagnostic matrix runs are retained: 222951-v26 stopped at a WSL forwarding
EOF on Redis restart; 223210-v26 exposed the intentionally unretried stale-connection
failure. The harness now retries transient readiness/shutdown control resets and
records bounded external recovery attempts. Application Redis retries remain disabled.

## Decision

Protection-first preserves the backend at the cost of availability on cache misses.
Available-local preserves more successful responses but can multiply backend work,
lose cross-instance coordination, and retain divergent local snapshots until TTL.
Both keep explicit ownership rejection out of L1. Admission caps limit cache work
and waiting clients separately; they do not turn leases into permanent locks or
bound the entire service. The broader workload comparison and soak remain V2.7.
