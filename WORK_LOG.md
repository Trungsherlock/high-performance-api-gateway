# Project Work Log

## Recording policy

This log records implementation work, design decisions, verification, and outstanding tasks. The initial entries are reconstructed from this development session and checked against the repository. Exact dates for individual earlier changes were not recorded, so the entries are organized by milestone rather than assigned dates.

Verification below describes checks run during implementation; creating the initial log did not rerun them. Passing ordinary tests does not establish that the race detector passes. Milestone 4 adds local benchmark results; these are not production capacity claims.

The original [project plan](high_performance_api_gateway_plan.md) remains the scope reference. Usage and current behavior are documented in [README.md](README.md). Performance measurements belong in [docs/results.md](docs/results.md).

## Milestone 0 — Foundation and experiment setup

**Status:** Implementation delivered; learning acceptance remains for the developer to demonstrate.

### Work completed

- Initialized the Go module and `.gitignore`.
- Added `cmd/mockbackend` with configurable address and artificial response delay.
- Added a deterministic public item response, `/healthz`, and `/stats` backed by an atomic request counter.
- Made the mock delay stop when the request context is canceled.
- Added the gateway entry point with configurable listening address, upstream URL validation, and a health endpoint.
- Added build/run/test instructions and a benchmark environment/results template.
- Created a separate system design reading guide before implementation.

### Verification recorded

- Backend tests covered deterministic responses, concurrent counter updates, and cancellation.
- Ordinary tests, vet, build, and formatting checks passed after resolving sandbox access for Go tooling.
- A live curl smoke test returned identical bodies for two item requests and `{"calls":2}` from backend stats.

### Notes

- The gateway initially returned 501 for forwarding routes; Milestone 1 replaced that scaffold behavior.
- Local Go version observed during setup: Go 1.25.7 on Windows/amd64.
- Understanding handlers, goroutines, mutexes, and contexts is a learning checkpoint that automated tests cannot certify.

## Milestone 1 — Baseline reverse proxy

**Status:** Implementation delivered; exercised baseline checks passed.

### Work completed

- Added a fixed-upstream reverse proxy using the standard HTTP reverse proxy.
- Reused a configured transport with connection pooling, connect/TLS timeouts, response-header timeout, and an outbound request deadline.
- Preserved methods, request bodies, escaped paths, raw query strings, supported headers, upstream statuses, and response bodies.
- Returned redirects to clients rather than following them.
- Used direct request dispatch to avoid automatic path cleaning on proxy routes.
- Kept `/healthz` local to the gateway and rejected CONNECT/protocol upgrades.
- Added error mapping for upstream connection failures (502) and timeouts (504).
- Added bounded graceful shutdown on interrupt/SIGTERM, with forced connection closure after the deadline.
- Documented an explicit cache-disabled baseline.

### Verification recorded

- Integration tests covered POST bodies, GET routing, escaped paths, query preservation, redirects, 404/500 forwarding, and hop-by-hop header stripping.
- Tests covered fixed-target routing, invalid configuration, connection refusal, timeout handling, and cancellation propagation.
- A shutdown test verified that the deadline closes an active request.
- Ordinary tests, vet, build, and formatting checks passed.
- A live gateway-to-backend smoke test returned the expected item, gateway health response, and one backend data call.

### Decisions and limits

- Incoming forwarded identity headers are removed; the gateway does not assert a trusted client identity.
- Once response headers are sent, a body failure aborts the stream rather than replacing the status with 504.
- Connection-pool limits are separate from the work limiter introduced in Milestone 3.

## Milestone 2 — Bounded LRU/TTL cache

**Status:** Implementation delivered; race-detector acceptance subsequently passed in Milestone 5.

### Work completed

- Added a custom cache using a map, doubly linked list, and one mutex.
- Added lazy expiration, LRU eviction, replacement accounting, and defensive copies of headers and bodies.
- Set limits of 1,000 entries, 16 MiB of retained payload bytes, and 64 KiB per cached response.
- Integrated the cache through a transport wrapper while retaining the proxy's forwarding behavior.
- Added conservative eligibility rules for public GET requests and complete explicitly public 200 responses.
- Included the exact upstream URL/raw query and accepted request header values in cache keys.
- Capped TTL at 10 seconds, subtracted upstream `Age`, and started freshness before the fetch.
- Bounded unknown-length buffering to 64 KiB plus one byte before falling back to streaming.
- Enabled caching by default and retained `CACHE_ENABLED=false` for comparisons.

### Verification recorded

- Unit tests covered LRU ordering, byte/count bounds, replacement accounting, expiration, defensive copying, and concurrent structural consistency.
- Policy tests covered public/private paths, unsupported headers and directives, response eligibility, TTL caps, and `Age` handling.
- Integration tests covered cache hits, distinct queries, expiration, cookies/auth, private responses, `Vary`, `Set-Cookie`, oversized bodies, incomplete bodies, upstream errors, POST bypass, and disabled caching.
- Ordinary tests, vet, build, and formatting checks passed.
- A live smoke test sent two identical requests through the gateway and observed one backend data call.

### Decisions and limits

- This is a restricted application cache, not a general RFC-compliant shared cache.
- The payload budget excludes keys, headers, active requests, copies, and runtime overhead.
- With coalescing disabled, oversized bodies stream from the original fetch without refetching.
- Race testing failed during the local cgo build, before race-instrumented tests ran.

## Milestone 3 — Coalescing and overload protection

**Status:** Implementation delivered; race-detector acceptance subsequently passed in Milestone 5.

### Work completed

- Added a custom per-key in-flight group with synchronous registration and context-aware waiting.
- Added a leader cache recheck to close the race between an initial miss and group registration.
- Gave shared fetches an independent bounded timeout so caller cancellation does not cancel healthy waiters.
- Added a lifecycle context to cancel shared work after gateway shutdown.
- Published shareable response data with separate headers and body readers per caller.
- Added a channel-based limiter covering all upstream traffic, including private/POST bypass and fallback fetches.
- Held permits through body consumption or close, with protection against releasing a permit twice.
- Added defaults of 32 active upstream operations and a 50 ms permit wait; exhausted waits return 503.
- Added `-coalesce`, `-max-concurrent`, and `-permit-wait` flags for experiments.
- Updated README configuration, cancellation behavior, and fallback tradeoffs.

### Verification recorded

- Group tests registered 100 overlapping calls and verified one execution, canceled-waiter isolation, shared errors, retry cleanup, and independence of distinct keys.
- Gateway tests covered 100-request cold/expired-key bursts, leader disconnects, timeout recovery, private-response fallback, and cookie bypass.
- An overload test held two backend operations active and issued 100 distinct requests; excess requests returned 503 while a cached hot response remained available.
- Limiter tests covered permit ownership through response-body consumption, double-release prevention, errors, and cancellation.
- The overload test was corrected from sequential requests to a concurrent distinct-key burst so it measured the intended condition.
- The timeout-recovery test was changed to wait for shared-flight completion directly, removing a timing assumption about client and shared deadlines.
- Final `go test ./... -timeout 30s`, `go vet ./...`, `go build ./...`, and formatting checks passed.
- Race detection was retried and still failed during the cgo build.

### Decisions and limits

- Cache hits and followers use no upstream permit.
- Uncacheable or oversized speculative responses are closed; each caller then performs a separate streaming fetch under the limiter. This adds a probe, even for one caller, and can return a later backend response. It avoids sharing private data but sacrifices deduplication on that path.
- If all callers disconnect, shared work may continue until its own deadline to fill the cache.
- The limiter bounds active upstream work and waiting duration; it does not bound inbound client count or total process memory.
- Disabling caching also disables coalescing. The limiter can remain enabled independently.

## Historical verification blocker — resolved in Milestone 5

`go test -race ./...` fails before running tests with:

```text
runtime/cgo: C:\Program Files\Go\pkg\tool\windows_amd64\cgo.exe: exit status 2
```

The observed environment has Go 1.25.7 and an MSYS2 GCC installation. At this stage the exact cause had not been established. Ordinary tests, vet, and builds passed, but these earlier failures did not constitute race-detector verification.

**Resolution:** Milestone 5 identified missing compiler-runtime PATH resolution. A process-local MSYS2 bin PATH adjustment made a minimal C compile and all package race tests pass, including a clean-source export verification. No global settings were changed.

## 2026-11-02 — Milestone 4: Benchmark, profile, optimize

**Status:** Implemented with local measurements. Race verification was blocked at this stage and subsequently passed in Milestone 5; wrk execution still requires tooling absent from the local WSL environment.

### Work completed

- Added cache Get/parallel Get/replacement and coalescing leader/join microbenchmarks.
- Added `cmd/bench`, a compiled closed-loop local runner covering direct backend,
  proxy-only, cache-only, and full modes; hot/mixed steady traffic and cold/expiry bursts.
- Recorded three final trials per mode for hot and mixed workloads, plus three
  cache-only cold, full cold, and full expiry bursts. Retained raw JSONL records.
- Added Windows whole-process CPU/RSS collection, percentile/error reporting,
  backend-call deltas, machine metadata, and source hashes.
- Added PowerShell reproduction/summary scripts and Linux wrk scripts for hot/mixed
  distributions, with separate warmup and three measured trials.
- Captured CPU and allocation profiles, preserving textual evidence and excluding
  profiled runs from the main comparison. Raw profile files are ignored by Git.
- Reused existing LRU nodes during replacement after identifying unnecessary list
  allocations. The identical replacement benchmark changed from 6 to 5 allocations
  and 1,568 to 1,520 bytes per operation; all original cache semantics remain tested.
- Added a reverse-proxy buffer pool after its copy buffer accounted for 72.82% of
  sampled allocated bytes in the full hot workload. Repeated the same load sweep
  and profile after the change; preserved before records.
- Added concurrent distinct large-response streaming coverage to check buffer reuse.
- Published environment, methodology, ranges, regressions, and limitations in
  `docs/results.md`; updated README and benchmark reproduction instructions.

### Findings and tradeoffs

- Cold bursts: cache-only made 100 backend calls; full mode made one in every
  recorded trial, with all 100 client requests successful.
- Warm full-mode median successful throughput changed from about 16,942 to
  28,875 requests/sec after copy-buffer pooling in short local trials. Reported
  ranges are wide; the measurements do not establish production capacity.
- Full mixed mode rejected about 10.44% with 503 and had lower successful
  throughput than cache-only, while reducing measured backend work. This is the
  limiter's availability tradeoff, not a universally positive performance result.
- Buffer reuse trades retained memory for reduced allocation churn. The native
  runner shares a process/runtime with both servers, so CPU/RSS are not gateway-only.
- Retained defensive copies and one cache mutex; the evidence did not justify
  weakening response isolation or adding cache sharding.
- The first `hot-before-*` exploration overlapped microprofiling and is excluded
  from comparisons. The later `before-pool-*` sweep was collected separately.

### Verification

- Ordinary tests, vet, and build passed after the implementation changes.
- Hot/mixed sweeps, cold/expiry bursts, replacement before/after benchmarks, and
  complete-workload before/after profiles were actually executed locally.
- PowerShell reproduction/summary scripts were exercised. The wrk shell script
  was syntax-checked in Ubuntu/WSL; wrk itself was not installed or executed.
- Validated all 33 canonical final trial records for trial counts, request totals,
  percentile ordering, and full-mode burst success/backend-call assertions.
- No new successful race-detector run is claimed. The cgo blocker recorded above
  remains outstanding.

## 2026-11-02 — Milestone 5: Polish and final verification

**Status:** Implemented and verified locally.

### Work completed

- Added compiled-binary five-minute setup instructions and a Mermaid request-flow
  diagram to README, plus links to design and final verification notes.
- Added `docs/design.md` explaining one-mutex LRU design, per-key stampede
  prevention, resource ownership, cancellation, overload/error behavior, HTTP
  limitations, and an evidence-backed interview explanation.
- Diagnosed the historical race-build blocker: GCC's helper programs required
  its MSYS2 runtime directory on PATH. Verified the diagnosis with a minimal C
  compile before and after the process-local adjustment.
- Passed `go test -race ./... -timeout 60s` across all test-bearing packages.
- Initialized local Git using the existing configured author identity and staged
  the source, documentation, and experiment records. Added LF checkout attributes
  for Go, Lua, and shell source.
- Exported staged source to a fresh temporary directory without existing binaries
  or profiles, then ran ordinary tests, race tests, vet, and builds successfully.
- Verified two live gateway requests returned deterministic bodies with one
  backend call. Reproduced three cold and three hot full-mode benchmark trials.
- Recorded reproduction JSONL separately from the original results and documented
  outcomes and limits in `docs/verification.md`.
- Prepared the final local commit; no remote repository publication was requested.

### Verification findings

- Cold reproduction: 100 successful requests and one backend call in each trial.
- Hot reproduction: zero measured backend calls/errors/rejections; median about
  36,105 successful requests/sec, with a wide 32,502–49,593 range.
- The benchmark variation supports retaining ranges and the shared-process
  limitation rather than presenting a single production capacity claim.
- Global environment settings were unchanged. The prior cgo blocker is resolved
  with the documented terminal-local setup.

## Optional follow-up work

- Recorded the implemented decisions, costs, evidence, and revisit conditions in
  [gateway tradeoffs](docs/tradeoffs.md), including a project-story outline.
  Cross-checked the numerical claims against `docs/results.md`; documentation only.

- Created [the v2 plan](high_performance_api_gateway_v2_plan.md) for stronger
  measurements, overload experiments, multiple gateways, shared caching, bounded
  leases, and failure injection. The plan was created before implementation; V2.0/V2.1 progress is recorded below.

- Consider longer, isolated-process or separate-machine measurements before making stronger performance claims.
- Install wrk/Go in a Linux environment if executing the optional wrk scenarios.
- Rehearse the request paths and tradeoffs; automated verification does not certify personal understanding.

## 2026-11-02 - V2.0 and V2.1

**Status:** Implemented and verified. Later V2 milestones remain planned.

- **Preservation:** Tagged the existing V1 snapshot `a567362` as `v1-baseline`;
  its implementation is unchanged from `465d9f4`. Used the existing V2 development
  branch. Original V1 plan and historical results were not modified.
- **Harness:** Added `cmd/loadgen` and `bench/run-v2.ps1`: three native processes,
  readiness checks, fresh state, metadata/source/binary fingerprints, captured
  logs, per-process CPU/RSS, watchdog, completion markers, and failure cleanup.
- **Visibility:** Added fixed-name JSON metrics on loopback administration,
  body-lifetime upstream gauges/histograms, cache/coalescing/admission counters,
  bounded sampled request-log queue, safe route classes, and readiness semantics.
- **Arrival accounting:** Added fixed-seed scheduled and closed-loop modes,
  bounded generator concurrency/connections, request deadlines, generator drops,
  scheduling lag, and scheduled/send latency percentiles. Warmup and measurement
  have separate before/after metric and backend snapshots.
- **Evidence:** Completed twelve 30-second steady trials after 5-second warmups,
  plus a 300-second soak. Preserved unchanged-server baselines and diagnostics.
  Raw records and reconciliation output are under `bench/results/v2/`;
  interpretation is in [V2 results](docs/v2/results.md).
- **Findings:** Same-rate full mode avoided about 80% of backend calls without
  increasing throughput when both modes kept up. Closed-loop full mode shed
  about 10.39% of clients. The soak served 150,000/150,000 requests with 30,031
  backend calls and zero drops/errors/rejections; active-work gauges drained to
  zero. Gateway RSS increased modestly, so flat-memory behavior is not claimed.
- **Corrections:** Fixed Windows process exit-code handling and transient artifact
  append sharing errors. Retained unsuccessful/interrupted runs outside canonical
  comparisons. Corrected analyzer semantics: one rejected flight can return 503
  to several followers; admission counters and client statuses are separate.
- **Verification:** Ordinary/race tests, vet, build, formatting, and diff whitespace
  checks passed. Tests cover burst counter reconciliation, shared error fanout,
  waiter cancellation/rejection cleanup, body ownership, blocked logging, and
  generator saturation. An intentional backend startup failure exited nonzero
  and left no child processes. Non-loopback admin binding was rejected.
- **Limits:** One Windows host, periodic resource samples, exact-percentile
  generator memory growth, fixed run order, and synthetic backend/workload.
  No Redis, multi-instance coordination, or bounded gateway waiter queue yet.
  See [V2 verification](docs/v2/verification.md) and [tradeoffs](docs/tradeoffs.md).

## Future entry template

### YYYY-MM-DD — Change title

- **Scope:** Requested task or milestone.
- **Changes:** Files and resulting behavior.
- **Decisions:** Tradeoffs and relevant constraints.
- **Verification:** Commands actually run and their outcomes.
- **Outstanding:** Failures, limitations, or work still required.
