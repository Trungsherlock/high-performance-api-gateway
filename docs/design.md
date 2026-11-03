# Design decisions and project explanation

## Purpose and scope

The gateway forwards ordinary HTTP requests to one configured backend. It avoids
repeated work for a deliberately narrow public API, shares overlapping eligible
misses, and bounds upstream concurrency. The mock backend makes request counts
and artificial service delays repeatable. This is a local systems/performance
project; it has not been operated as a production service.

## Why one cache mutex

The map, recency list, and byte accounting form one invariant. Even a cache hit
updates the LRU list, so an RWMutex would still need exclusive access on that
path. A single mutex keeps operations easy to review. Network IO occurs outside
the lock, and returned data is copied after releasing it.

The profiles showed avoidable response-copy buffer allocations and replacement
list-node allocations. They did not isolate the cache mutex as a reason to shard.
Runtime lock samples in a whole-process profile cannot be attributed directly to
this cache. Sharding also changes global LRU/byte-budget behavior, so it would
need a separate measured justification.

## How stampede prevention works

1. A request must satisfy the original and rewritten request eligibility rules.
2. A fresh cache hit returns immediately, without registering a flight or taking
   an upstream permit.
3. On a miss, the request registers or joins the per-key in-flight call. Registration
   happens synchronously under the group's short-lived mutex.
4. The leader rechecks the cache, covering a fill completed between the initial
   miss and registration, then performs one bounded upstream operation.
5. Only a complete response admitted to the cache becomes a shared result. Each
   caller gets its own headers and body reader over immutable published bytes.
6. Completion publishes the result/error, removes the registry entry, and releases
   waiters. A future miss may start a new flight.

The shared operation has its own timeout and lifecycle context. A caller leaving
only cancels its wait. If all callers leave, the operation may continue to fill
the cache until its deadline. Gateway shutdown subsequently cancels shared work.

An HTTP error, private response, oversized body, or other unshareable result from
the speculative fetch is closed. Each caller then performs an independent
streaming request. This adds a probe and may observe a later backend response;
the implementation favors a simple, safe publication boundary over deduplicating
every possible response. Requests already known to be private bypass speculation.

## Limits and ownership

| Resource | Bound / owner | Release or cleanup |
|---|---|---|
| Stored response bodies | 16 MiB total; 64 KiB per entry | LRU eviction, replacement, or lazy expiration |
| Stored entries | 1,000 | Same as above |
| Unknown-size fill buffer | 64 KiB plus one byte before fallback | Response completion/close |
| Active upstream operations | 32 by default | Body EOF/read error or close; network errors release immediately |
| Queued permit waiters | 32 by default since V2.2; -1 selects the legacy unbounded policy | Acquisition, cancellation, or wait expiry |
| Permit waiting duration | 50 ms by default | Acquire, timeout, or caller cancellation |
| Shared fetch lifetime | Upstream timeout, 10s by default | Completion, deadline, or lifecycle cancellation |
| Graceful shutdown | 5s | Close remaining server connections after deadline |
| Streaming copy buffers | Reused through sync.Pool | Returned after copying; pool can discard at GC |

Keys, headers, defensive copies, goroutines, inbound connections, flight records,
and pooled buffers are outside the cache payload budget. Waiting duration and queued permit waiters are bounded in the V2.2 CLI default.
Per-key followers, inbound connections, and legacy-mode waiter count remain
unbounded. There is no total RSS guarantee.

## Overload, latency, and error rate

The limiter counts active work, not requests per second. It applies to cache
misses, bypass requests, and fallback requests, and holds a permit through the
upstream body rather than releasing it when headers arrive. Cached responses and
followers consume no permit.

Queueing can absorb a short burst, but excessive waiting raises tail latency and
retains resources. A full permit queue or expired wait sheds load with 503. The response header
`X-Gateway-Rejection` distinguishes `queue_full` and `wait_timeout`. Those responses must count
as failures, even if total completed responses/sec increases. The recorded mixed
workload illustrates this: full mode protected the backend but had about 10.44%
rejections and lower successful throughput than cache-only.

## Failure semantics

- Backend connection/transport failure: 502.
- Backend timeout before headers: 504.
- Permit wait expires: 503.
- Actual upstream 404/500: forwarded, never cached. Under speculative coalescing,
  forwarding occurs through the independent fallback described above.
- Incomplete eligible buffered response: no cache entry; return the transport/read
  error through the proxy's error mapping.
- Body failure after headers were already forwarded: abort the stream; its status
  cannot be replaced with a new gateway error code.
- Request cancellation: stop that request's work/wait. A shared operation remains
  bounded independently for the other callers.

## What the measurements establish

Cold bursts showed 100 backend calls without coalescing versus one with it, with
all clients successful. Buffer reuse removed a dominant allocation source and
improved median warm-key throughput in short local trials. LRU replacement now
uses one fewer allocation. See [results](results.md) for raw evidence and ranges.

The experiment does not establish production capacity, gateway-only CPU/RSS,
distributed consistency, or resilience across multiple gateway instances. The
runner, backend, and gateway share one process. Timings vary, the backend delay is
synthetic, and a closed-loop generator reduces offered arrivals when responses slow.

## HTTP and product limitations

- One fixed upstream, HTTP/1.1 behavior, no CONNECT/WebSockets or service discovery.
- Restricted public-response cache; no general Vary, revalidation, invalidation,
  stale serving, or full HTTP caching freshness implementation.
- Strict header eligibility intentionally bypasses many real client requests.
- No authentication/authorization platform, tenant quotas, distributed cache,
  automatic retries, persistence, or production observability stack.
- Cache, coalescing, and limiter state are local to a single gateway process.

## Suggested interview explanation

"I built a Go reverse proxy to study repeated backend work and overload. I started
with a forwarding baseline, added a bounded LRU/TTL cache, then coordinated
overlapping misses and limited upstream concurrency. I tested cancellation and
failure cleanup, measured backend calls and successful throughput separately,
and used allocation profiles to justify buffer and list-node reuse. The results
also showed that load shedding can protect the backend while lowering availability."

Be ready to draw the request paths, explain the unshareable-response fallback,
distinguish cache capacity from process memory, and describe the limits of the
measurement setup. Use only results supported by the recorded experiments.


## V2.3 process boundary

The optional front selects one probe-healthy gateway once per request. It does not
reroute a failed request. Health checks own readiness; caller cancellations do not
mark instances unhealthy. Check observations can become stale before forwarding,
and short probe deadlines can exclude an otherwise-running instance.

Each gateway owns its own LRU, flight group, permit pool, and queued-waiter pool.
The harness compares one 32-permit instance with two 16-permit instances, keeping
aggregate configured permits and queued slots at 32. This is not a distributed
limiter or cache: a controlled cold burst causes one fill per reached instance.
See [V2.3 evidence](v2/multi-instance.md) for restart resets, front connection-pool
queueing, resource attribution, and readiness/availability observations.
