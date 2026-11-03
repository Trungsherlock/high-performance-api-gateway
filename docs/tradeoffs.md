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
