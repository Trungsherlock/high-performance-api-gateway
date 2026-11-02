# High-Performance API Gateway in Go — Project Plan

**Working title:** High-Performance API Gateway (name optional)  
**Project type:** Focused systems / backend performance engineering  
**Language:** Go  
**Environment:** Linux or Ubuntu on WSL2; everything runs locally  
**Estimated size:** ~18–23 source, test, script, and documentation files  
**Estimated effort:** ~20–35 focused hours over 2–3 weeks (adapt to Go experience)  
**Status:** Design and implementation plan, not a claim that features or performance results already exist

---

## 1. Executive summary

Build a **single-upstream HTTP reverse proxy** that protects a mock backend from repeated requests and bursts. The project is deliberately limited to **three core engineering skills**:

1. **Concurrency:** goroutines, locks, cancellation, in-flight request coalescing, and bounded downstream concurrency.
2. **Caching:** a custom, bounded **LRU + TTL** in-memory cache with correct synchronization and narrow cache eligibility rules.
3. **Performance engineering:** controlled benchmarks, race testing, CPU/heap profiling, and **measured** optimization of latency and throughput.

A successful project is not defined by a huge RPS claim or many gateway features. It is defined by **correctness under concurrent load**, a **repeatable benchmark**, and clear explanation of what changed and why.

### Core problem

A backend serving the same expensive public resource repeatedly wastes work. When a popular key expires, many simultaneous clients may all trigger downstream requests (**cache stampede**). An overloaded backend can then have rising latency and failures.

**Goal:** Reduce duplicate downstream work and bound downstream concurrency, without leaking private responses or creating unbounded memory use.

### What this is *not*

No AWS deployment, Kubernetes, distributed cache, authentication platform, persistent storage, complex routing, Grafana/Prometheus, OpenTelemetry, rate-limit product, service discovery, or full HTTP caching RFC implementation. A **small mock backend and local load testing** are sufficient.

---

## 2. Explicit scope and requirements

### Functional scope (MVP)

| Feature | Required behavior | What is intentionally excluded |
|---|---|---|
| HTTP reverse proxy | Route requests to one configurable upstream; preserve path/query/method/status and supported headers | Multiple backends, dynamic discovery, WebSockets, CONNECT tunneling |
| LRU cache | O(1)-average `Get`/`Put`/eviction with a hash map + doubly linked list | Redis, disk persistence, distributed coherence |
| TTL | Each entry expires after a bounded duration; expired entries never serve as hits | Background cleanup daemon (lazy expiration is sufficient) |
| Request coalescing | Concurrent eligible misses for the same key use one in-flight fetch | Sharing across gateway processes |
| Concurrency limit | At most N distinct downstream fetches in flight; bounded wait or fast rejection | Rate limiting per user, adaptive control loops |
| Benchmarks | Compare same workload with proxy-only, cache, and coalescing/limiter modes | Public deployment or guaranteed 10,000 RPS |
| Profiling | Find and document at least one actual bottleneck or explain why an optimization was not justified | Optimizing by guesswork |

### HTTP boundary (keep this project realistically small)

- Support **ordinary HTTP/1.1 requests** through one upstream. Use Go's standard HTTP stack; do *not* hand-write an HTTP parser.
- The cache is an **application-specific public-response cache**, not a general-purpose RFC 9111-compliant HTTP cache.
- **Only `GET` requests to an allowlisted prefix** (e.g., `/api/public/`) may be cached or coalesced; all other traffic is proxied without caching.
- Only cache completed **`200 OK`** responses that explicitly advertise `Cache-Control: public, max-age=<positive seconds>` and pass safety checks.
- Use a per-entry **maximum response size** (initially 64 KiB), an **entry-count limit** (initially 1,000), and a **payload-byte budget** (initially 16 MiB). Exceeding a cap must not silently buffer an entire large response in RAM.
- Disable caching and coalescing for requests with `Authorization`, `Cookie`, `Range`, conditional request headers (`If-*`), `Cache-Control: no-cache` / `no-store`, or otherwise variant-sensitive conditions that the implementation does not explicitly support. These requests still pass through the proxy normally.
- Do not store responses with `Set-Cookie`, `Vary` (any value for this MVP), `Cache-Control: private`, `no-store`, `no-cache`, or `max-age=0`. Do not store errors or incomplete bodies.
- Avoid forwarding or storing hop-by-hop headers (including those nominated by the `Connection` header). Document any unsupported proxy semantics; tests must cover the supported subset.
- Do not expose an open proxy. Route only to the configured upstream target.
- Coalesce only requests satisfying the same narrow cache eligibility checks. Non-cacheable requests must never share a response through this mechanism.

**Design note:** These restrictions are conservative on purpose. General shared HTTP caching requires a much larger protocol surface (e.g., `Vary`, validators, `Age`, freshness calculations, partial responses, auth rules). Restricting the demo to an explicitly public API lets the systems engineering work remain substantial while keeping correctness reviewable.

---

## 3. Architecture

```text
               GET /api/public/items/42
                         |
                      Client(s)
                         |
                         v
                 +----------------+
                 | Gateway :8080  |
                 |   eligibility  |
                 +--------+-------+
                          |
                 +--------+---------+
                 |                  |
           non-cacheable        eligible GET
                 |                  |
                 v                  v
           reverse proxy        LRU + TTL
                 |                /    \
                 |             HIT    MISS
                 |              |       |
                 |           respond    v
                 |                 in-flight group
                 |                       |
                 |                bounded limiter
                 |                       |
                 +----------+------------+
                            |
                            v
                    Mock API :9000
                            |
                         response
```

### Recommended implementation strategy

- **Baseline / bypass:** use `net/http/httputil.ReverseProxy` (not a handmade HTTP parser), with a shared, configured `http.Transport`.
- **Eligible cache path:** implement a narrowly scoped buffered fetch using the same transport / `http.Client` (or a carefully designed transport wrapper). Cache only complete, bounded responses. Preserve headers and status correctly; **do not** use `httptest.ResponseRecorder` as a production buffering layer.
- **Response value:** an immutable struct containing status, safe cloned headers, and copied body bytes. Each client gets its own header copy / write operation.
- **Miss path:** cache check → join/create in-flight fetch → acquire downstream permit in the leader only → fetch with timeout → validate and (if eligible) store → distribute immutable result → release waiters.
- **Oversized or uncacheable downstream response:** do not keep it in memory. Ensure all waiting callers still receive correct responses; a simple MVP choice is to have the leader return a `bypass` marker and let each caller use the normal streaming proxy path. Record the tradeoff that this sacrifices deduplication for oversized responses.
- **Failure path:** if the shared fetch errors, release all waiters with a defined gateway error; remove the in-flight entry so a future request can retry.

### Example local endpoints

| Endpoint | Mock backend behavior | Purpose |
|---|---|---|
| `GET /api/public/item?id=42` | 200, JSON, `Cache-Control: public, max-age=10`, optional 50 ms delay | Primary cache/coalescing benchmark |
| `GET /api/public/item?id=99` | Different public data | Distinct cache key test |
| `GET /api/private/profile` | Per-user-like content, `Cache-Control: private` | Must not cache |
| `POST /api/update` | Echoes request | Proxy bypass correctness |
| `GET /api/public/large` | Body larger than 64 KiB | Size-limit / no-caching test |
| `GET /api/public/error` | 500 or configurable 2 s delay | Error / timeout tests |

---

## 4. Internal design decisions

### A. Custom LRU + TTL

**Core types (sketch, not required exact Go API):**

```go
type Entry struct {
    StatusCode int
    Headers    http.Header
    Body       []byte
    ExpiresAt  time.Time
}

type Cache struct {
    mu        sync.Mutex
    items     map[string]*list.Element
    order     *list.List // front = most recently used
    maxItems  int
    maxBytes  int64
    usedBytes int64
}
```

- On `Get`: lock → find → discard if expired → move to front → return **immutable/copy-safe** result → unlock.
- On `Put`: lock → replace old entry if any → account for payload bytes → insert at front → evict oldest until **both** count and byte budgets hold → unlock.
- No network I/O, logging-heavy processing, or response serialization while holding the LRU lock.
- Expiration is **lazy** on access. A background sweeper is not needed for v1.
- Start with **one mutex**, not sharded locks; introduce sharding only if profiling proves contention.
- Payload budget is not equal to process RSS (map/list metadata and Go runtime use additional memory). Measure both.

**Cache key:** for this project, use the exact normalized upstream route + raw query string (e.g., `GET|/api/public/item?id=42`); include the fixed upstream identity if configurable across runs. Do not merge or canonicalize query parameters unless equivalence is proven. Only permit one explicitly public representation (see header restrictions above).

### B. Coalescing (“singleflight” behavior)

Create your own small per-key in-flight registry to learn concurrency:

```text
map[cacheKey]*call
call = { done chan struct{}, result Response, err error }
```

- A leader creates the call; followers wait on `done` instead of contacting the backend.
- Under a mutex, only perform short map operations. Never hold it during I/O.
- On completion, set immutable result/error, close `done`, and remove the call so future misses are allowed.
- Follower cancellation: `select` on `done` or caller context. A canceled follower must not cancel everyone else.
- Leader cancellation: give the shared upstream operation its **own bounded timeout**, so one disconnect cannot unexpectedly abort healthy waiters. Never run detached requests without a timeout.
- TTL expiration bursts and simultaneous cold misses are the important tests.
- Read [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) as a **reference design**, but implement the small group yourself first.

### C. Downstream concurrency limiter

- A buffered channel semaphore (capacity N) is enough; start at **N=32**.
- Acquire only for a request that is about to call the upstream. **Cache hits and coalesced followers do not consume permits.**
- Add a short, bounded permit-wait budget (e.g., 50 ms). On timeout, return `503 Service Unavailable`; record this as overload shedding, not successful throughput.
- Release in a `defer` to avoid leaked permits on all paths.
- Independently set upstream connect/response timeouts and a total outbound deadline.
- Do not conflate a semaphore for in-flight work with requests-per-second rate limiting.

### D. Proxy transport and lifecycle

- Reuse **one** `http.Transport` with keep-alives and appropriate per-host connection limits; transports are safe for concurrent use.
- Use `ReverseProxy.Rewrite`/`SetURL` for the bypass route; validate upstream URL at startup.
- Use a limited, explicit set of forwarded request headers in the cached path; strip hop-by-hop headers from requests and responses.
- Keep error distinctions intelligible: upstream networking error → `502`, upstream timeout → `504`, gateway overload → `503`. Forward actual upstream HTTP status codes, including 500, without caching them.
- Support SIGINT/SIGTERM shutdown with an `http.Server.Shutdown` deadline. The process should not hang indefinitely when closing.

---

## 5. Implementation roadmap (incremental milestones)

**Rule for every milestone:** write tests before proceeding; preserve a working version at every checkpoint. Avoid prematurely adding extra features.

### Milestone 0 — Go foundation + experiment setup (2–4 h)

**Learn:** `net/http`, request handlers, goroutines, `sync.Mutex`, `context.Context`, `go test`, and `httptest`.

**Implement:**

- [ ] Initialize Go module, directory structure, README, and `.gitignore`.
- [ ] Add `cmd/mockbackend` with deterministic response body, artificial delay, and an atomic count of downstream calls.
- [ ] Add `cmd/gateway` with configurable listening address and upstream URL.
- [ ] Define benchmark machine/environment recording template (OS, CPU, Go version, concurrency).

**Acceptance:**

- [ ] Backend starts locally and `curl` returns deterministic responses.
- [ ] You can explain what a handler, goroutine, mutex, and context do.

### Milestone 1 — Correct baseline reverse proxy (3–4 h)

**Learn:** HTTP proxy semantics, upstream transport reuse, cancellation, timeouts.

**Implement:**

- [ ] Use `httputil.ReverseProxy` for one upstream.
- [ ] Correct method/path/query, relevant end-to-end headers, status, and response body forwarding.
- [ ] Configure a shared transport, bounded timeouts, error responses, shutdown.
- [ ] Add mode `CACHE_ENABLED=false` for clean baseline experiments.

**Tests:**

- [ ] GET with query preserved; POST body proxied; upstream 404/500 forwarded unchanged.
- [ ] Hop-by-hop `Connection` headers not forwarded; backend unavailable → `502`.
- [ ] Request cancellation does not leave work hanging indefinitely.

**Acceptance:** baseline proxy behaves correctly with caching disabled.

### Milestone 2 — Custom bounded LRU cache (4–6 h)

**Learn:** map + doubly linked list, eviction complexity, TTL, memory bounding, concurrency safety.

**Implement:**

- [ ] Implement cache API (`Get`, `Put`, stats if useful) from scratch using `container/list` and a map.
- [ ] Evict by item count and payload byte budget; implement lazy TTL expiration.
- [ ] Add conservative **cache policy** for explicitly public GET responses.
- [ ] Integrate cache-hit response path and cache-fill path for small, completed 200 responses.
- [ ] Never cache a response before its body has been read successfully and verified.

**Tests:**

- [ ] LRU ordering and eviction; overwrite updates accounting; expired item misses.
- [ ] Different query strings produce different keys.
- [ ] Cookie/auth/private/no-store/Vary/oversized/non-200 responses bypass cache.
- [ ] Multiple concurrent reads/writes keep internal structures consistent.

**Acceptance:** two sequential requests for a cacheable key yield one backend call; after TTL expiry, a subsequent request reaches backend again. `go test -race ./...` passes.

### Milestone 3 — Coalescing and overload protection (4–6 h)

**Learn:** per-key synchronization, cancellation semantics, semaphore backpressure.

**Implement:**

- [ ] Add custom per-key in-flight group; make its result immutable and shareable.
- [ ] Re-check cache inside the leader after becoming leader (protects the race between miss and group creation).
- [ ] Add bounded upstream concurrency semaphore and bounded permit wait.
- [ ] Add shared upstream timeout, waiter cancellation, cleanup on success/error/timeout.

**Tests (make these deterministic with barriers/channels):**

- [ ] Launch 100 requests for one cold key concurrently; backend observes exactly **one** eligible small-response fetch while requests overlap.
- [ ] Launch 100 requests for 100 *distinct* cold keys; upstream active calls never exceed N.
- [ ] Cancel one follower: remaining requests succeed.
- [ ] First upstream call fails: waiting calls get error, in-flight record clears, subsequent request can retry.
- [ ] Cache TTL expiration under burst: coalescing prevents stampede.
- [ ] Cache hit does not wait behind an overloaded upstream limiter.

**Acceptance:** no deadlocks, no leaked permits or stuck waiters, race detector clean.

### Milestone 4 — Benchmark, profile, optimize (5–8 h)

**Learn:** load-generation methodology, p50/p95/p99 interpretation, CPU/heap/lock profiles, bottleneck-driven changes.

**Implement:**

- [ ] Add documented `wrk` scenario scripts for fixed keys and mixed key distributions.
- [ ] Add `go test -bench=. -benchmem` microbenchmarks for LRU and in-flight group.
- [ ] Capture benchmark results for each design mode in `docs/results.md`.
- [ ] Profile CPU, allocations/heap, and optionally mutex/blocking under a reproducible hot workload.
- [ ] Choose **one actual bottleneck** and apply a justified improvement; rerun the identical experiment.

**Acceptance:** publish baseline and final results, workload parameters, unsuccessful experiments, and tradeoffs. Improvements are measured; no invented RPS or percentages.

### Milestone 5 — Polish and write-up (2–4 h)

- [ ] Document how to build/run/test in under five minutes.
- [ ] Include a diagram of request flow, cache eligibility policy, and known HTTP limitations.
- [ ] Explain why a single mutex was retained or why sharding became necessary.
- [ ] Explain how stampede prevention works and how overload shedding affects latency/error rate.
- [ ] Commit clean final code and benchmark reproduction commands.

**Definition of done:** You can describe the code and design tradeoffs in a systems/backend interview and reproduce the key benchmark without relying on cloud infrastructure.

---

## 6. Suggested repository structure

```text
api-gateway/
├── cmd/
│   ├── gateway/
│   │   └── main.go
│   └── mockbackend/
│       └── main.go
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── gateway/
│   │   ├── handler.go
│   │   ├── handler_test.go
│   │   ├── proxy.go
│   │   └── proxy_test.go
│   ├── cache/
│   │   ├── lru.go
│   │   ├── lru_test.go
│   │   ├── policy.go
│   │   └── policy_test.go
│   ├── coalesce/
│   │   ├── group.go
│   │   └── group_test.go
│   └── limiter/
│       ├── limiter.go
│       └── limiter_test.go
├── bench/
│   ├── run.sh
│   └── hot-key.lua        # optional wrk mixed/hot-key script
├── docs/
│   └── results.md
├── go.mod
├── README.md
└── .gitignore
```

Approximately **20–22 tracked files**, depending on the benchmark script, tests, and whether dependencies add a `go.sum`. Structure is guidance, not a quota: merge files if that makes the code clearer. Avoid creating abstractions before they are needed.

---

## 7. Benchmark methodology (critical for resume credibility)

### Four main modes

1. **Direct backend:** measures backend cost, *not* a direct apples-to-apples comparison of proxy overhead.
2. **Proxy only:** gateway enabled, cache and coalescing off (**primary baseline**).
3. **Cache only:** warm-cache benefits; coalescing off.
4. **Cache + coalescing + limiter:** final implementation.

### Required workloads

| Scenario | Setup | What to learn |
|---|---|---|
| Warm/hot key | 1 cacheable key, warm cache, e.g., 100 concurrent connections | Cache-hit throughput and latency |
| Many distinct keys | Large keyspace / low hit ratio | Miss-path overhead and downstream saturation |
| Cold same-key burst | 100 requests released simultaneously via test barrier | Actual backend request count / stampede prevention |
| TTL expiry burst | Warm → wait for TTL → burst | Expiration correctness and synchronized miss behavior |
| Slow backend | 50–200 ms synthetic service time | Queueing, tail latency, bounded in-flight calls |
| Backend failure | 500, timeout, connection refusal | Correct status/errors, recovery and no cache poisoning |

**Example commands** (run once the corresponding paths are implemented):

```bash
# terminal 1
go run ./cmd/mockbackend

# terminal 2
CACHE_ENABLED=false go run ./cmd/gateway

# terminal 3 — initial smoke check
curl -i 'http://127.0.0.1:8080/api/public/item?id=42'

# steady-load example; repeat using identical workload parameters
wrk -t4 -c100 -d30s --latency 'http://127.0.0.1:8080/api/public/item?id=42'

# correctness and microbenchmarks
go test ./...
go test -race ./...
go test ./internal/cache -bench=. -benchmem
```

**Benchmark hygiene:**

- Benchmark a compiled binary for final comparisons (`go build` then run), not `go run` in a race-enabled or debug configuration.
- Warm up before recording a steady-state interval. Separately measure cold starts/misses.
- Repeat each configuration at least three times; report median and range, not only the best run.
- Keep hardware, runtime version, connection count, backend delay, payload size, and cache-hit distribution the same.
- If gateway, mock backend, and load generator share one machine, state that CPU competition affects measurements.
- Record **achieved throughput (successful requests/sec), p50/p95/p99 latency, error/503 rate, cache-hit ratio, downstream call count, CPU, and memory**. A high RPS number with many rejections is not success.
- `wrk` is a **closed-loop** generator; `-c100` means connections, not an offered fixed RPS. **10,000 RPS is an exploratory stretch target, not a contractual acceptance criterion.**

### Template for `docs/results.md`

```markdown
## Environment
- CPU / cores:
- RAM / OS / WSL2 or bare Linux:
- Go version:
- Backend delay / response size:
- Cache capacity / TTL / concurrency limit:
- wrk threads / connections / duration / warmup:

## Results (3+ trials per configuration)
| Mode | Successful RPS | p50 | p95 | p99 | Error/503 % | Backend calls | RSS |
|------|----------------|-----|-----|-----|-------------|---------------|-----|
| Proxy only | TBD | TBD | TBD | TBD | TBD | TBD | TBD |
| Cache only | TBD | TBD | TBD | TBD | TBD | TBD | TBD |
| Full gateway | TBD | TBD | TBD | TBD | TBD | TBD | TBD |

## Profiling hypothesis and evidence
- Initial bottleneck:
- Profile evidence:
- Change made:
- Before vs after (same setup):
- Regression / tradeoff:
```

### Profiling checklist

- `go test -bench=. -benchmem` for allocations per operation.
- Generate CPU profile with `go test -bench=. -cpuprofile=cpu.out` and heap profile with `-memprofile=mem.out`; view with `go tool pprof`.
- For live gateway profiling, expose `net/http/pprof` **only on localhost and only when profiling is explicitly enabled** (not a public endpoint).
- Use mutex or block profiling if a profile suggests lock contention; avoid adding sharded caches before evidence.
- Optimization examples *only if evidence supports them*: reduce repeated string/header copying, preallocate buffers appropriately, reduce critical-section work, or shard hot-key locks.

---

## 8. Correctness, concurrency, and failure test matrix

| Test | Expected result |
|---|---|
| Cache miss, then hit | Two sequential eligible GETs → one upstream call |
| TTL expiry | Expired entry never returned; next call refetches |
| LRU eviction | Most recently accessed survives when capacity exceeded |
| Byte capacity | Payload bytes remain at or under cap |
| Parallel reads+writes | No data races, map/list remain consistent |
| Distinct query keys | No accidental mixing of responses |
| Authorization/Cookie | Never stored or coalesced; each request proxied separately |
| `no-store`, `private`, `Vary`, `Set-Cookie` | Never stored |
| POST or nonallowlisted GET | Normal proxy, no cache |
| Oversized/partial body | No cache entry, correct safe fallback |
| 500/timeout on fill | No poisoned cache; waiters terminate; future retry allowed |
| 100 same-key cold requests | One upstream call while they overlap (eligible bounded response) |
| N+1 distinct cold requests | Concurrent upstream count <= N; bounded wait/rejection |
| Leader/follower cancellations | No leaked work/permits; healthy followers not abandoned |
| Restart | Empty cache; no persistence promised |
| SIGTERM | Bounded graceful shutdown without indefinite hanging |

**Useful test techniques:** `httptest.NewServer`, atomics for downstream-call count, barriers/channels for simultaneous starts, controlled fake clock for TTL unit tests (inject `now func() time.Time`), and time-limited tests to detect deadlocks. The race detector only checks executed paths; intentionally exercise concurrency.

---

## 9. Key tradeoffs to document

1. **Mutex vs RWMutex vs sharding:** LRU `Get` updates recency, so even hits mutate the list. Start with a mutex; measure before changing architecture.
2. **LRU vs LFU:** LRU is simpler, constant-time, and appropriate for a bounded project; LFU is out of scope.
3. **Cache vs coalescing:** Cache removes repeat work across time; coalescing removes duplicate *simultaneous* work during misses.
4. **Limiter vs backpressure:** A bounded semaphore prevents excess upstream concurrency, but rejected requests still affect user-visible availability. Measure rejection rate honestly.
5. **Buffering vs streaming:** Small public responses can be buffered safely within a cap; streaming large/private responses should bypass cache.
6. **Freshness vs speed:** Short TTL may improve freshness but raise miss volume; long TTL improves hit rate but risks stale content.
7. **Application-specific vs RFC-compliant caching:** Narrow, intentional policy is safest for this exercise; do not represent it as a general HTTP cache.
8. **Go vs POSIX:** This project teaches Go runtime concurrency and HTTP performance, **not direct C/POSIX pthread/fork programming**.

---

## 10. Completion checklist and resume readiness

### Must be true before calling it complete

- [ ] A mock backend and gateway run locally with documented commands.
- [ ] Baseline proxy correctness tests pass.
- [ ] LRU eviction, TTL, memory caps, and cache bypass policies pass tests.
- [ ] Cold same-key bursts coalesce correctly; distinct-key calls obey limiter.
- [ ] Cancellation, timeouts, errors, and oversized responses are handled deliberately.
- [ ] `go test ./...` and `go test -race ./...` pass under exercised concurrent test cases.
- [ ] At least three repeated benchmark runs per principal configuration are documented.
- [ ] Profiling identifies one concrete improvement opportunity (or justifies keeping the simpler design).
- [ ] README documents limitations and includes a small architecture diagram.

### Resume bullet template — fill in *measured* values only

> Built a concurrent API gateway in Go with a custom bounded LRU/TTL cache, singleflight-style request coalescing, and downstream concurrency limits; reduced **[measured backend calls]** under burst load and improved **[measured p99 latency / throughput]** versus a no-cache proxy baseline.

Do not include improvements unless backed by the reproducible results in `docs/results.md`. If performance gains are mixed, describing the technical correctness and measured tradeoffs is still valid.

---

## 11. Learning resources and source references

The plan is based on current official documentation and public reference implementations; resources are linked for deep study, not as code to copy blindly.

1. **Go ReverseProxy / ProxyRequest / Rewrite:** https://pkg.go.dev/net/http/httputil  
   Read before Milestone 1; Go already implements many complex proxy semantics.
2. **Go HTTP Transport:** https://pkg.go.dev/net/http#Transport  
   Covers connection pooling, reuse, max connections, and timeouts.
3. **Go data race detector:** https://go.dev/doc/articles/race_detector  
   Use throughout concurrent milestones.
4. **Go singleflight:** https://pkg.go.dev/golang.org/x/sync/singleflight  
   Reference design for duplicate in-flight call suppression.
5. **HTTP caching standard (RFC 9111):** https://www.rfc-editor.org/rfc/rfc9111.html  
   Especially shared-cache restrictions for auth, `private`, and `no-store`; explains why this project deliberately restricts cacheability.
6. **Go profiling guide:** https://go.dev/blog/pprof  
   How to collect/interpret profiles.
7. **Go testing / benchmark API:** https://pkg.go.dev/testing  
   Microbenchmarks, subtests, and test hygiene.
8. **wrk benchmark tool:** https://github.com/wg/wrk  
   Load generation and latency reporting; note its closed-loop model.

### Suggested study order

`net/http` → Go mutex + map/list → TTL and cache safety → custom per-key flight group → semaphore + context/timeouts → `go test -race` → `wrk` and `pprof`.

---

## 12. Starting point: your first development session

1. Create a new repository named `go-api-gateway` (name can change later).
2. Install a current stable Go release and `wrk` inside Linux/WSL2.
3. Implement only the mock server with a counter and a configurable artificial delay.
4. Implement a single-upstream `httputil.ReverseProxy` and write two proxy integration tests.
5. Run `curl` and record the first **proxy-only baseline**. **Do not start on the cache until the baseline is correct.**

The first meaningful deliverable is **a tested proxy-only baseline**, not a large codebase.
