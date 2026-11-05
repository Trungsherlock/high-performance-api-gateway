# V2.5: bounded cross-instance fill leases

V2.5 keeps local coalescing and adds optional per-key Redis leases. Under healthy
Redis, a supported public response and sufficient lease lifetime, two gateways
can share one cold fill. A lease is temporary authority to publish a response;
it does not guarantee exactly-once backend execution.

## Request path

```text
fresh L1 hit -> return immediately
local miss -> join local flight
  local leader -> read shared response
    hit -> promote with existing expiry
    miss -> SET lease NX PX with random token
      owner -> recheck shared response -> bounded backend fill
            -> Lua: verify token, publish response, delete lease
            -> warm L1 only after acknowledged publication
      nonowner -> bounded jittered polling -> read result or acquire expired lease
```

Only one local leader per key contacts Redis for its local flight. Other local
callers retain independent cancellation. Canceling the client that initiated a
flight does not cancel the shared operation or another gateway's healthy waiter.

The owner rechecks shared data after acquisition to close the miss/acquire race.
It then fetches under the existing local backend limiter. The deadline is the
minimum of the configured fill deadline, overall shared-work deadline, and
acquisition-start plus lease duration minus the safety margin. Lookup, body read,
and publication are included. Acquisition elapsed time reduces usable authority;
pauses and clock/Redis failure can still invalidate assumptions.

## Ownership and rejected results

- Lease namespace: `gateway:lease:v1:<same response-key hash>`. Tokens contain
  32 cryptographically random bytes, hex encoded; tokens and keys are not logs or
  metric labels. Lease acquisition is atomic SET NX with expiration.
- Publication is one Lua operation: compare the current token, reject an expired
  response, SET the response with its original PXAT expiry, then delete the lease.
  A separate GET followed by SET would leave a race and is not used here.
- Release is another token-checked Lua operation. An expired owner cannot delete
  a replacement owner's lease. Successful publication already consumes its lease;
  the owner's deferred release then becomes a harmless no-op.
- Backend results remain outside both caches until publication succeeds.
  Rejected ownership returns **503 lease_lost** to that local flight. This project
  chooses to discard the old result even for its original caller. A response from
  the new owner may be read and promoted by a subsequent request.
- An ambiguous publication error also returns 503 and does not warm L1. Redis
  may nevertheless have committed the value; another request can discover it.
- Cleanup uses a bounded Redis operation independent of the expired fill context.
  A crashed process cannot clean up; expiration permits another owner to proceed.
- Private, oversized and otherwise unshareable responses retain the existing
  individual streaming fallback, under the local limiter. They are not distributed
  shared results. Repeated speculative probes across gateways remain possible.

All participating writers must enable leases when testing coordinated fills.
V2.4's unconditional writers share the response namespace and do not honor leases;
mixing policies is not a supported ownership experiment.

## Configuration and failure policy

Leases are opt-in: add `-leases` alongside `-cache-mode layered` or `shared`.
Local coalescing must remain enabled; invalid combinations fail startup.

| Flag | Default | Meaning |
|---|---|---|
| `-lease-duration` | 3s | finite lease TTL, without renewal |
| `-lease-fill-timeout` | 2s | owner recheck, backend/body, publication budget |
| `-lease-wait` | 4s | nonowner polling/acquisition budget |
| `-lease-poll` | 25ms | jittered delay in [25ms, 50ms) by default |
| `-lease-protocol` | v2 | added in V3.2. `v2` uses one read-or-acquire script and skips the release after an acknowledged publish. `v1` is the V2.5 sequence and was the default before V3.2 was accepted; see [V3.2](../v3/lease-protocol.md) |

The CLI safety margin is 250ms. Fill timeout must be strictly less than lease TTL
minus margin. Lease/wait durations are capped at one minute; poll base is 1ms to
1s. The overall upstream/shared-work timeout still applies. An owner acquired near
the wait deadline gets a separate bounded fill budget inside that overall deadline.

Fresh L1 hits do not depend on Redis. In lease mode, a coordination dependency
failure returns **503 coordination_error**, and an expired wait returns
**503 coordination_timeout**. Backend errors/timeouts retain 502/504 handling.
A nonowner does not bypass the lease and issue an uncoordinated speculative fill.
V2.4 modes without `-leases` retain their available-local fallback. Expanded policy
selection and dependency failure experiments remain V2.6.

New counters include lease acquisitions, publications, rejected publications,
releases, polls, errors, wait timeouts and distributed followers. A distributed
follower here is a contending **local leader**, not every client waiting behind it.
The distributed_waiters gauge follows those leaders while waiting; coordination_wait
measures the time to shared hit/acquisition/error, excluding an owner's backend fill.
The cache_dependency histogram also includes lease operations. Existing dependency
and read/write error counters remain visible. Acquisition count can exceed fill
count when the post-acquisition recheck finds an already completed response.

Wait duration and Redis connections are bounded. The number of local followers,
flights for distinct keys, and inbound connections still need separate admission
policies; this milestone does not claim a total-memory bound.

## Reproduction

Use pinned Redis 7.4.2 and go-redis v9.7.3. For these lease experiments use an isolated
64 MiB Redis with **noeviction**, no persistence and loopback binding. This avoids
ordinary cache eviction removing a live lease. Data still expires at its TTL;
capacity pressure produces explicit errors rather than evicting ownership keys.
The V2.4 allkeys-lru experiment remains historical; do not silently reuse that policy
when claiming lease retention. maxmemory bounds the dataset, not total Redis RSS.

With a working Docker daemon, start Redis in a separate terminal:

```powershell
docker run --rm --name gateway-v25-redis -p 127.0.0.1:6379:6379 redis:7.4.2 redis-server --save "" --appendonly no --maxmemory 64mb --maxmemory-policy noeviction
```

```powershell
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/burst.exe ./cmd/burst
python bench/run-v25.py --redis-addr 127.0.0.1:6379
$env:REDIS_TEST_ADDR = '127.0.0.1:6379'
go test ./... -count=1 -timeout 60s
```

The harness creates fresh gateway/backend processes per trial, allocates loopback
ports, records commands/PIDs, binary/source hashes and runtime settings, asserts
counter reconciliation, and stops its owned children on success or failure. Redis
is externally owned and is not flushed or stopped by the harness. Linux uses
extensionless binaries under bin/ with the same Python harness. Real Redis tests
explicitly skip without REDIS_TEST_ADDR; fake tests cannot prove Lua atomicity.

## Collected evidence

[Final nine trials](../../bench/results/v2/20261103-220609-v25/trials.jsonl),
[summary](../../bench/results/v2/20261103-220609-v25/summary.json),
[manifest](../../bench/results/v2/20261103-220609-v25/manifest.json), and
[Redis configuration](../../bench/results/v2/20261103-220609-v25/redis.json).
Redis ran in WSL2; two gateways, backend and burst generator ran as separate native
Windows processes on the same host. Each gateway retained 16 permits and 16 queue
slots (aggregate 32/32). There was no front proxy in these correctness experiments.

| Scenario, three trials each | Backend attempts each trial | Observed result |
|---|---|---|
| Shared responses, leases disabled | 2 | 100 successful equal-body responses, 50 per gateway |
| Shared responses, leases enabled | 1 | 100 successful equal-body responses, one publication |
| Kill the lease owner during fill | 2 | interrupted owner's client loses connection; survivor succeeds |

Both burst cases used a 500ms backend delay and finished in roughly 0.52-0.54s.
The reduction is in duplicate backend work, not a demonstrated latency improvement.
The killed-owner case used a 1s lease, 600ms owner deadline, 2s wait, 10-20ms polling
and 100ms dependency operations. Survivor completion after the kill took 1.498,
1.483 and 1.487s, including lease expiry and its 500ms fetch. Each kill happened
while the owner had acquired a lease and started the backend, before publication.
The expected owner transport error is retained in every trial. Two attempts mean
one abandoned attempt plus the survivor's fill, not exactly-once execution.
Survivor/burst work gauges drained, dependency errors were zero, and counters
reconciled. These are short correctness experiments, not capacity or soak evidence.

Real Redis tests cover concurrent acquisition, abandoned-owner expiry, old-owner
publication/release rejection with different response versions, one fill across
100 overlapping clients, cancellation isolation, no L1 admission after lost
ownership, owner timeout/release/recovery, the acquire/recheck race and streaming
fallback. Focused tests cover bounded nonowner waiting, coordination failure,
fresh L1 availability and invalid options.

The complete ordinary/race suites, vet and build passed with real Redis enabled;
three final focused tests also passed ordinary/race checks. Logs:
[ordinary](../../bench/results/v2/v25-tests.txt),
[race](../../bench/results/v2/v25-race.txt),
[final focused ordinary cases](../../bench/results/v2/v25-extra-tests.txt),
[final focused race cases](../../bench/results/v2/v25-extra-race.txt).
Use the documented MSYS2 compiler PATH for Windows race tests. Runtime source and
binary hashes pin the experiment. Final validation corrected the rejection reason
when the wait deadline expires during acquisition; the focused test verifies
coordination_timeout. Successful experiment paths were unchanged. The final source
hashes and validation note are in [final-validation.json](../../bench/results/v2/20261103-220609-v25/final-validation.json).

## Guarantees and limits

This is one Redis authority with expiring ownership tokens, not consensus, Redlock,
backend fencing or a strict distributed concurrency limiter. A pause, expiry,
Redis restart, eviction or partition can allow duplicate backend execution.
Token checks protect publication relative to Redis's current ownership; they do
not undo backend work or make side effects exactly once. No renewal, automatic
backend retry loop, Pub/Sub, Redis Cluster or failover is introduced. Cache expiry
retains V2.4's same-host/aligned-clock assumption.

See the official [ownership-token guidance](https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/)
and [Lua atomic execution semantics](https://redis.io/docs/latest/develop/programmability/eval-intro/).
The expanded Redis outage, delay, restart, memory pressure and degradation matrix
is V2.6; this milestone verifies bounded waiting and ownership transitions first.
