# Gateway tradeoffs and project story

This records implemented v1 decisions and the evidence behind them. V2 decisions
remain proposals in the [v2 plan](../high_performance_api_gateway_v2_plan.md).
See [design details](design.md) and [measurement methods and results](results.md).

## 1. Local caching: backend work versus freshness and memory

**Decision:** Keep eligible public responses in a bounded in-process LRU cache,
with a maximum TTL of 10 seconds, 1,000 entries, and 16 MiB of body payloads.

**Benefit:** Repeated requests avoid the backend and its service time.
**Cost:** Clients can receive older data within the freshness window. Cache data
and defensive copies consume memory; the payload budget is not a process RSS cap.
Local cache state also disappears on restart and is not shared across instances.

**Evidence:** Warm cache/full trials made zero measured backend calls. This
demonstrates avoided work for the chosen workload, not a universal speedup.

**Revisit when:** The API needs immediate invalidation, more varied representations,
or coordination across gateway instances.

## 2. Request coalescing: fewer backend calls versus lifecycle complexity

**Decision:** Share overlapping eligible misses per key. Give the shared operation
its own bounded lifetime so one caller's cancellation does not cancel other callers.

**Benefit:** A popular cold key can require one backend fetch instead of many.
**Cost:** Coordination needs cleanup on success, failure, cancellation, and timeout.
Work can continue until its deadline even if every caller disconnects.

Only complete cache-admitted responses are shared. An unshareable speculative
response is closed and callers fetch independently. This creates an extra probe
and can return a later backend response; known private requests bypass the probe.

**Evidence:** In each recorded cold-burst trial, 100 requests succeeded. Cache-only
made 100 backend calls; full mode made one. Expiry bursts in full mode also made one.

**Revisit when:** Speculative fallback traffic becomes expensive or multiple
gateways need to coordinate misses.

## 3. Concurrency limits: backend protection versus request availability

**Decision:** Allow 32 active upstream operations by default and wait up to 50 ms
for a permit. Hold the permit through response-body consumption. Reject expired
waits with 503; cache hits avoid permits.

**Benefit:** Bounds active backend work, including bypass and fallback traffic.
**Cost:** Requests can fail during overload. Bounded waiting time does not bound
the number of waiters, incoming connections, or total gateway memory.

**Evidence:** Mixed full mode had a median 1,312 backend calls versus 2,596 for
cache-only, but about 10.44% of requests were rejected and successful throughput
was lower (5,894 versus 6,224 requests/sec). These fixed-duration closed-loop runs
also completed different request counts; the call reduction is not a pure measure
of deduplication or proof of optimal limiter settings.

**Revisit when:** Measurements justify different limits or bounded queue capacity.

## 4. Narrow cache eligibility: understandable correctness versus coverage

**Decision:** Cache only a restricted public GET path and compatible request and
response headers. Bypass authenticated, cookie-bearing, varied, and other
unsupported representations according to the implemented policy.

**Benefit:** Makes safe response reuse easier to reason about and test.
**Cost:** Many valid real-world requests receive no caching benefit. This is a
restricted application cache, not a complete HTTP caching implementation.

**Revisit when:** A concrete use case requires additional representations,
revalidation, or broader HTTP cache semantics.

## 5. One cache mutex: simple invariants versus possible contention

**Decision:** Protect the map, recency list, and byte accounting with one mutex.
Keep network operations outside the lock. Cache hits update recency, so they also
need exclusive access.

**Benefit:** Keeps global LRU ordering and capacity accounting straightforward.
**Cost:** Concurrent cache operations can contend. Sharding could reduce contention
but complicates global eviction and capacity distribution.

**Evidence:** Profiles justified allocation changes, but did not establish this
mutex as a bottleneck. Sharding has not been demonstrated necessary.

**Revisit when:** Cache-specific contention measurements show a material limit.

## 6. Allocation reuse: less allocation versus ownership and retained memory

**Decision:** Reuse LRU nodes on replacement and pool 32 KiB proxy copy buffers.
Preserve defensive cache copies and isolate buffers between concurrent streams.

**Benefit:** Reduces repeated allocation work.
**Cost:** Buffer reuse requires clear ownership. Pooled memory is outside the cache
payload budget, can remain retained, and can be discarded by garbage collection.

**Evidence:** LRU replacement dropped from six to five allocations and saved 48
bytes per operation. Proxy buffer reuse removed a dominant allocation source.
Short before/after throughput trials improved, but their variation and execution
order limit causal performance claims; see the full results.

**Revisit when:** Larger streaming workloads or memory profiles reveal a different
buffer-size or retention tradeoff.

## A story supported by the implementation

> I built a gateway to study repeated backend work and overload. I added a bounded
> cache, coordinated overlapping misses, and limited upstream concurrency. Cold
> bursts showed that coalescing could reduce 100 backend calls to one. Mixed
> traffic showed the cost of protection: fewer backend calls came with rejected
> requests and lower successful throughput. I then used allocation profiles to
> choose focused optimizations while keeping cache correctness and failure cleanup
> explicit.

The experiments use a synthetic backend and a closed-loop generator sharing the
gateway's process. They do not establish production capacity or gateway-only CPU
and memory costs. Warm full-mode throughput was lower than cache-only in the
recorded runs, but the variation does not isolate coordination overhead as the
cause. Present observations separately from explanations that still need testing.

V2 extends the story by testing whether shared caching and cross-instance
coordination justify their dependency, latency, and failure-handling costs.


## V2.0-V2.1 decisions

- **Separate processes on one host:** isolate runtime and CPU/RSS attribution,
  at the cost of process lifecycle work. OS scheduling and hardware remain shared;
  this does not reproduce separate-machine network behavior.
- **Scheduled and closed-loop generators:** scheduled arrivals expose demand that
  continues during slow responses; closed-loop tests describe fixed concurrent
  clients. They answer different questions and need separate comparisons.
- **Bound generator in-flight work and drop excess arrivals:** prevents the load
  tool from silently building an unlimited request queue. Drops reduce delivered
  load and must be reported separately from gateway rejection. Scheduling lag is
  recorded rather than assuming the generator keeps perfect time.
- **Exact client percentiles and bounded server histograms:** the generator keeps
  phase samples for exact nearest-rank percentiles, using memory proportional to
  sent requests. Server histograms use fixed buckets and constant metric state,
  trading precision for bounded storage. Generator memory is measured separately.
- **Loopback JSON metrics with one mutex:** keeps the project dependency-free and
  telemetry easy to inspect. Instrumentation adds synchronization and allocation
  overhead; it is not a complete production monitoring integration. Snapshots copy
  metric state before writing so slow admin clients do not hold the metric lock.
- **Sampled asynchronous request logs:** a bounded queue limits pending records
  and drops logs when full. Sampling and process exit can lose records, and log
  files still need external retention if used for long-running service operation.

- **Count operations and client outcomes separately:** a permit rejection counts
  one failed admission, while a coalesced error can return 503 to many clients.
  This makes layer boundaries visible, at the cost of requiring explicit metric
  definitions rather than assuming every counter should match client counts.

These are implemented measurement choices. Redis and lease tradeoffs remain
proposals until their later milestones are implemented and tested.

## V2.2: bounded upstream permit waiting

The CLI caps queued permit waiters at 32, alongside 32 active upstream operations
and a 50 ms wait budget. Rejecting excess work immediately avoids retaining an
arbitrary number of queued upstream requests, at the cost of rejecting bursts
that a larger queue might absorb. Timeout bounds duration; queue capacity bounds
count. Neither bounds followers that wait for a shared fetch or inbound clients.

Queue admission is not FIFO: a newly arriving request may acquire a released
permit before an older waiter. This keeps the implementation small and avoids
promising fairness; wait deadlines bound starvation duration. A FIFO scheduler
would require additional ownership and cancellation rules.

The old policy remains available with `-max-waiters -1`. `0` removes permit
queueing and gives immediate rejection under saturation. Queue-full and wait
expiry have distinct fixed reasons in headers, metrics, and sampled logs.
See [the admission experiments](v2/admission.md) for measured outcomes and why
these defaults are a local policy choice rather than a production capacity claim.


## V2.3: independent instances and an extra routing hop

- **Local state per gateway:** keeps each cache and flight group simple and
  independent, but duplicates cold fills across reached instances. Controlled
  100-request bursts made two backend calls even though local coalescing worked.
- **Fixed aggregate backend budget:** comparing 1x32 with 2x16 avoids silently
  doubling backend pressure. A surviving node retains 16 permits after its peer
  stops; there is no automatic budget redistribution or global enforcement.
- **Best-effort health probes:** skip observed-unhealthy targets and allow rejoin,
  but observations can become stale. One failed probe on a running gateway caused
  59 front-level 503s in the study. Its exact failure cause was not retained;
  detection speed and false exclusion remain a tuning tradeoff.
- **One target selection with no alternate-target retry:** makes failures visible
  and avoids application replay/load amplification, at an availability cost if a
  chosen target has just stopped. Standard transport recovery may reconnect to
  the same selected target; this is not a wire-level exactly-once guarantee.
- **Connection pooling at the front:** reuses connections, but its own connection
  ceiling can create a hidden queue ahead of gateway admission. The initial
  64-connection pool caused substantial queueing and generator drops. A ceiling
  of 256 per target lets the study's 256-client limit reach gateway admission.
  It does not bound all front memory or solve admission above that client count.
- **Extra routing process:** provides one entry point and readiness observations,
  while adding CPU, memory, latency, and another failure point. Separate-process
  measurements expose those costs; two gateways did not double throughput with
  the same aggregate backend budget.

See [V2.3 topology and results](v2/multi-instance.md). Shared caching and leases
remain later milestones, not properties of this implementation.

## V2.4: sharing responses before coordinating fills

- **Shared storage first:** completed responses cross gateway boundaries; cold
  fills still overlap. This isolates storage correctness from lease correctness.
- **Three modes:** local avoids a dependency; shared exposes its round-trip cost;
  layered adds independent copies to keep warm traffic off Redis. The nine-trial
  [path comparison](v2/shared-cache.md) reports counters and short sequential latency.
- **Bounded best effort:** Redis operations have deadlines, capped connections and
  no retries. Errors fall back to local coalescing/limits, retaining availability
  while potentially increasing backend duplication. Readiness stays local.
- **Conservative freshness:** atomic value/PTTL reads, elapsed-time subtraction,
  original expiry and PXAT prevent promotion or pool delay restarting TTL. Absolute
  expiry assumes aligned clocks in this single-host experiment.
- **Restricted serialization:** versioned, size-bounded allowlisted response values
  favor safe reuse over arbitrary extension-header preservation. Invalid values
  miss; command failures have separate error counters. Keys are hashed, payloads
  remain readable to Redis operators.
- **Disposable single authority:** 64 MiB maxmemory with allkeys-lru and no
  persistence limits cache retention, not total server RSS. No lease/fencing or
  concurrent-writer ordering guarantee exists yet.

## V2.5: bounded leases instead of permanent cross-instance ownership

- **Two levels of sharing:** local flights keep each key's local clients off
  Redis; a short lease suppresses speculative fills across gateways. Three
  100-client trials reduced backend attempts from two to one with leases enabled.
- **Expiring authority:** no renewal; owner deadline is shorter than the lease.
  Killed owners recover by expiry. In three process trials the survivor completed
  in 1.48-1.50s, including a 500ms backend fetch. The interrupted client's transport
  error and the abandoned backend attempt remain in the report.
- **Atomic ownership checks:** publication and release compare random tokens in
  Lua. A delayed owner cannot overwrite the newer cached version or delete the
  newer lease. This protects Redis publication, not exactly-once backend work.
- **Reject before local admission:** rejected/ambiguous publication returns 503,
  including to the owner's original callers. Keeping L1 empty for that result
  gives up availability for an explicit ownership boundary. Fresh L1 hits still
  work without Redis; lease-dependent misses fail closed on coordination errors.
  V2.4 nonlease modes keep their available-local fallback.
- **Polling over Pub/Sub:** bounded jittered polling is simple and cancellation
  aware but adds Redis operations and handoff latency. It bounds wait duration,
  not total waiter count or process memory. A post-acquisition recheck avoids a
  redundant fill if publication races with acquisition.
- **noeviction for lease trials:** keeping live leases out of ordinary eviction
  trades eviction-based cache availability for capacity errors. Data TTLs free
  space; Redis restart or manual deletion can still erase authority. This differs
  deliberately from the historical V2.4 allkeys-lru cache-only experiment.
- **Scope:** no consensus, globally strict limiter, renewal, cross-machine clock
  guarantee or backend fencing. Expanded degraded policies remain V2.6. See
  [lease design, reproduction and evidence](v2/leases.md).

## V2.6: availability versus backend protection during coordination failure

- **Explicit policy:** protection-first rejects lease-dependent misses; available-local
  falls back to local coalescing/limits. Fresh L1 hits bypass Redis in both modes.
  In the race-instrumented matrix, three 100-client failure bursts per policy
  produced zero backend calls/100 rejections under protection-first, versus two
  backend calls/100 successes under available-local for each fault.
- **Local fallback is deliberately uncoordinated:** it writes only L1. Two local
  limiters can admit their summed capacity, and copies may diverge until TTL.
  There is no hidden unconditional shared write or globally strict backend limit.
- **Ambiguous publication:** available-local may deliver the completed safe response
  to current local callers, but never warms L1 or refetches after a failed reply.
  Explicit lost ownership still returns 503 in both modes. A subsequent shared
  lookup can discover a publication that actually committed.
- **Bound waiting and work separately:** CLI defaults cap cache-miss clients at
  1024 and registered distinct flights at 64 per instance. Canceling clients frees
  their slots while shared work keeps a flight slot until completion/deadline.
  These caps shed load and are not total memory/inbound-connection bounds.
- **No silent retries:** a Redis restart can fail the first read on a stale pooled
  connection. Retaining that 503 and explicitly probing bounded recovery tells a
  more accurate story than claiming error-free restart. Available-local's recovery
  response can instead trigger duplicate work. Fresh-key reuse verifies reconnection.
- **Evidence scope:** isolated real Redis faults also ran with a race-instrumented
  gateway; OOM was made deterministic, and failed harness assumptions were kept.
  Short correctness checks are not production capacity/soak evidence. See
  [V2.6 policies, limits and fault results](v2/faults.md).

## V2.7: judge mechanisms by workload and measured cost

- **Matched backend budgets:** compared 1 x 32 against 2 x 16 permits, with the
  same aggregate queues/cache admission and a front hop in every mode. More nodes
  still add independent L1 capacity and process overhead; this is not equal-RAM scaling.
- **Storage versus coordination:** twelve controlled cold bursts made one fill
  with one local node, two with independent nodes/shared storage, and one with
  leases. In the mixed steady workload, unique keys dominated backend calls;
  shared storage alone did not reduce fills, while leases added dependency work.
- **Fixed-rate evidence:** three 30-second trials per mode at 500 arrivals/sec
  yielded roughly 499 successful RPS, with trial latency/resource ranges reported.
  This is not a maximum-throughput speedup claim. Redis/leases have measured cost
  even when most traffic hits L1. Cleanup checks after acknowledged publication
  are included in the lease operation counts.
- **Keep imperfect outcomes:** one shared-mode generator drop and one leased-mode
  coordination 503 remain in principal results. The five-minute soak sent 150,000
  requests with 149,997 successes and three coordination 503s. Lower backend counts
  caused by rejection are not all credited as successful duplicate suppression.
- **Resource ownership:** gateway early/late RSS medians increased modestly; the
  generator's retained samples grew more. Redis INFO excludes the surrounding WSL
  VM/kernel, and sampled CPU/RSS is not exact full-phase CPU or true peak memory.
  One five-minute workload cannot prove leak freedom or production readiness.
- **Reproducibility over restarting a long study:** a readiness parsing interruption
  was preserved; resume checked application/binary hashes and kept 22 completed
  trials, then finished two cold cases and the soak. Source-only rebuild and ten
  fault reproductions passed. All underlying causes are qualified rather than
  assuming every dependency error was a timeout.

See [final measured ranges and architecture](v2/final.md) and
[per-process resource evidence](v2/final-resources.md). This is a tested
infrastructure project, not production employment or an exactly-once service.
