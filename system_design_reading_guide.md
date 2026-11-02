# System Design Reading Guide

This guide covers what to understand before implementing the API gateway. It focuses on system behavior and design decisions, without language or framework preparation.

The goal is to understand enough to explain each request path, identify resource limits, and predict failures. You can learn protocol details as you implement each milestone.

## 1. Reverse proxies and HTTP boundaries

Start with this flow:

```text
Client -> Gateway -> Backend
Client <- Gateway <- Backend
```

The gateway acts as a server to the client and as a client to the backend. It owns two separate connections and must deliberately carry request and response information between them.

### Learn

- The difference between a reverse proxy, a forward proxy, and an API gateway.
- Request methods, paths, query strings, headers, and bodies.
- Response status codes, headers, and bodies.
- End-to-end headers versus hop-by-hop headers, including headers named by `Connection`.
- Streaming versus buffering: streaming forwards data as it arrives; buffering holds data before forwarding it.
- Connection reuse and why opening a new connection for every request adds work.
- The difference between a backend returning an HTTP error and the gateway failing to reach the backend.

### Read

- [HTTP Semantics, RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html): use sections 7, 9, and 15 as references for forwarding, methods, and status codes. Do not read the entire specification before starting.
- Project plan sections 2 and 3: identify the supported HTTP boundary and trace the bypass path.

### Be able to explain

1. What must the gateway preserve when forwarding a POST request?
2. Why should an upstream redirect normally be returned to the client?
3. Why can a streaming response use less memory than a buffered response?
4. What is the difference between an upstream 500 response and a gateway 502 response?

## 2. Cache correctness, freshness, and eviction

A cache reuses an earlier result. Its first design question is whether reuse is correct. Eviction policy comes afterward.

### Learn

- Cache hits, misses, and the cache-aside pattern: check the cache, fetch on a miss, then store an eligible result.
- Cache keys: which inputs identify the response, and which requests must remain separate.
- Representations: one URL can produce different content because of authentication, cookies, language, encoding, or other headers.
- Public versus private responses and the purpose of `Cache-Control`, `Vary`, and `Set-Cookie`.
- TTL and freshness: how long a stored response may be reused.
- Expiration versus eviction: expiration removes permission to reuse stale data; eviction frees capacity.
- LRU: remove the least recently accessed entry when capacity is exceeded.
- Entry-count limits, per-response limits, and total payload-byte budgets.
- Why retained cache bytes are only part of total process memory.

### Read

- [HTTP Caching, RFC 9111](https://www.rfc-editor.org/rfc/rfc9111.html): sections 2–4 for cache operation, storage, reuse, and freshness; section 5.2 for cache directives.
- Project plan sections 2 and 4A: connect the narrow public-response policy to the cache design.

This project intentionally implements a restricted application cache. Use the RFC to understand the restrictions; full HTTP cache compliance is outside the scope.

### Be able to explain

1. Why can two requests with the same URL require different responses?
2. Why is a response with `Set-Cookie` unsuitable for this project's shared cache?
3. What happens when an entry expires but remains allocated until the next access?
4. Why are both count and byte limits useful?
5. What freshness assumptions does the mock backend need to satisfy?

## 3. Cache stampedes and request coalescing

When a popular entry expires, many clients can miss at once. Without coordination, each client starts the same backend work. This is a cache stampede.

Coalescing gives overlapping requests for one key a shared operation. One request becomes the leader; followers wait for its result.

### Learn

- The difference between caching across time and sharing work during an overlapping miss.
- Per-key coordination: unrelated keys should not wait for the same fetch.
- Leader and follower lifetimes.
- Request eligibility versus response shareability: response safety is known only after the backend replies.
- Why a follower disconnect should not cancel work needed by other callers.
- Why shared work needs its own bounded deadline.
- Cleanup after success, failure, timeout, and cancellation.
- Rechecking the cache after becoming leader to avoid a redundant fetch.

### Read

- Project plan sections 4B and 8: work through the coalescing behavior and concurrent failure scenarios.
- The request flows at the end of this guide: draw the leader and followers separately.

### Be able to explain

1. What happens when 100 requests arrive for one uncached key?
2. What happens if the backend returns an unshareable response?
3. What happens if the original client disconnects while followers remain?
4. How does a failed flight allow the next request to retry?

## 4. Overload, queueing, and resource bounds

A backend has finite capacity. When demand exceeds the rate at which work finishes, unfinished work accumulates unless the system rejects or limits it.

### Learn

- Arrival rate, completion rate, service time, concurrency, and queueing time.
- Concurrency limiting versus rate limiting: one bounds active work; the other controls arrivals over time.
- Bounded waiting versus immediate rejection.
- Backpressure and load shedding.
- Why waiting requests still consume resources.
- Why a timeout bounds waiting duration but does not by itself bound the number of waiters.
- Why every upstream path must obey the intended concurrency boundary, including bypass traffic.
- Why permits should cover the full upstream operation, including response-body consumption.

### Useful relationship

Under stable conditions, Little's Law relates averages:

```text
Average requests in the system = Throughput × Average time in the system
```

For example, 200 requests per second spending an average of 0.1 seconds in the system implies about 20 requests present on average. This is a relationship between measured averages, not a safe capacity formula for bursts or an unstable queue.

### Read

- Project plan sections 4C, 7, and 9: connect limiter behavior to latency and rejection measurements.
- [Google SRE: Handling Overload](https://sre.google/sre-book/handling-overload/): focus on bounded queues and load shedding.

### Be able to explain

1. If the backend gets ten times slower, where does unfinished work accumulate?
2. Why can an unlimited queue make an overloaded system worse?
3. Why should a cache hit avoid waiting for an upstream permit?
4. Which resources remain unbounded after limiting active backend calls?

## 5. Failure, deadlines, and cancellation

Design the failure paths as explicitly as the successful path. Every operation needs an owner, an end condition, and a cleanup rule.

### Learn

- Connection failures, response-header timeouts, body-read failures, and incomplete responses.
- Total operation deadlines versus individual connection or read timeouts.
- Client cancellation versus the lifetime of a shared backend fetch.
- Why incomplete responses must not enter the cache.
- Why errors must release waiters, permits, and in-flight records.
- Graceful shutdown: stop accepting work, allow bounded completion, then terminate remaining work.
- Retry amplification: automatic retries can increase load during a failure. Retries are not required for this MVP.

### Read

- Project plan sections 4D and 8: assign a cleanup rule to each failure scenario.
- [Google SRE: Addressing Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/): focus on how overload and failures spread between components.

### Be able to explain

1. What should happen if a response body fails halfway through?
2. Which deadline applies while waiting for a permit, and which applies to the backend fetch?
3. How do healthy followers survive another client's cancellation?
4. What ensures shutdown finishes within a bounded time?

## 6. Performance experiments and interpreting results

An optimization is useful only in relation to a workload and a measurable objective.

### Learn

- Baselines and controlled comparisons: change one mechanism at a time where practical.
- Successful throughput versus total completed responses, including rejections.
- Median latency and tail latency: p50, p95, and p99.
- Hit ratio, backend call count, active upstream calls, CPU, and memory.
- Warm-cache tests versus cold misses and expiration bursts.
- Repeated trials and reporting variation.
- Closed-loop load: clients wait for responses before sending more work.
- Open-loop load: requests arrive according to an external schedule, potentially exceeding capacity.
- Why a closed-loop test can reduce its arrival rate when the server slows down.
- Why synthetic backend delay demonstrates avoided work but does not prove benefits for every real application.

### Read

- Project plan section 7: use the workloads and results template as the experiment outline.
- Project plan section 9: predict the tradeoffs before collecting results.

### Be able to explain

1. Why does a warm single-key test say little about miss-path performance?
2. Why can low latency with many 503 responses be misleading?
3. How will you distinguish the benefit of coalescing from the effect of the limiter?
4. Which workload might make caching add overhead with little benefit?

## Before coding: five design exercises

Draw each flow and mark who owns the work, who waits, what consumes memory, what is bounded, what can fail, and who releases resources.

1. **Cache hit:** return a valid stored response without contacting the backend.
2. **Cold miss:** fetch one small public response, validate it, store it, and reply.
3. **Same-key burst:** coordinate 100 overlapping misses and handle one canceled follower.
4. **Distinct-key overload:** send many unique misses to a slow backend; show waiting and rejection behavior.
5. **Unshareable response:** receive a large or private response during a shared fetch; show how the leader and followers get correct responses.

## How much to learn before starting

Study reverse proxy behavior and cache correctness first. Then gain enough understanding of coalescing, overload, and failure handling to explain the five exercises.

You do not need to master every reference before implementing the baseline proxy. Revisit each topic before its milestone. Distributed consensus, multi-region systems, service discovery, and distributed cache coherence are outside this project's preparation requirements.
