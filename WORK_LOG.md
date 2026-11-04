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

## 2026-11-02 - V2.2 bounded admission and overload study

**Status:** Implemented and verified. V2.3 and later remain planned.

- **Implementation:** Added independent queued-permit capacity, with CLI default
  32; zero rejects under saturation and -1 selects legacy unlimited waiting.
  Acquiring, canceling, or timing out releases the queue slot; acquired permits
  still cover upstream body lifetime. Existing library constructors preserve
  legacy semantics, and cache hits still bypass admission.
- **Visibility:** Added distinct queue-full/wait-timeout errors, 503 reason header,
  fixed reason counters, sampled log reason, backend active/peak/delay/uptime stats,
  and generator rejection counts plus three-second scheduled-start cohorts.
- **Experiments:** Extended the separate-process harness with admission controls,
  unique misses, timed slowdown/recovery, backend samples, and build reuse. Added
  `bench/run-v22.ps1` and its summarizer. Collected 15 four-second screening cases
  and 24 repeated six/nine-second trials, all after one-second warmup. No V1 or
  V2.0/V2.1 result files were overwritten.
- **Findings/default:** Retained 32 permits and 50 ms wait, adding 32 queued slots.
  Under healthy overload, bounded and legacy modes each delivered about 631
  successful requests/sec; observed queue peaks were 32 versus 61. Smaller queues
  traded slightly more rejection for lower tails; larger queues did not add
  meaningful throughput. Recovery returned toward healthy capacity. RSS varied,
  so no fixed memory saving or total-memory cap is claimed.
- **Verification:** Ordinary/race tests, vet, build, formatting, and whitespace
  checks passed. Deterministic tests covered a 200-request burst (8 queued,
  192 rejected), cancellation and reuse, reason distinctions, cache-hit bypass,
  and slowdown selection. All 39 trials reconciled with zero generator drops or
  transport errors; backend peaks respected permit limits, capped sampled queues
  respected configured bounds, and final work gauges drained to zero. No harness
  child processes remained.
- **Correction:** Initial smoke test exposed a backend argument initialization
  mistake in the harness edit; fixed it and retained the successful rerun. Smoke
  attempts are excluded from the study index.
- **Limits:** Short synthetic trials on one host; no fairness guarantee, bounded
  follower admission, inbound connection cap, or per-waiter memory estimate.
  Default selection is a conservative local policy, not universal tuning.
  See [full policy and evidence](docs/v2/admission.md) and
  [tradeoffs](docs/tradeoffs.md).

## 2026-11-03 - V2.3 independent gateway instances

**Status:** Implemented and verified. V2.4 and later remain planned.

- **Implementation:** Added health-aware round-robin `internal/front` and
  `cmd/frontproxy`, with dedicated loopback administration, per-target probe/
  selection/error state, bounded forwarding/probe deadlines, and no alternate-
  target application retry. Independent probes own health; caller cancellation
  cannot mark an instance unhealthy. Gateway cache/coalescing logic is unchanged.
- **Harness:** Added `cmd/burst`, multi-admin generator snapshots,
  `bench/run-v23.ps1`, a one-command study wrapper, and strict reconciliation.
  Captured per-process metadata/resources, source/binary hashes, front state,
  restart resets, and all child cleanup. Compared aggregate budgets of 1x32 and
  2x16 permits, with the same total 32 queued slots.
- **Evidence:** Recorded 18 final canonical trials: three direct bursts, three
  front bursts, three stop/restart cases, and nine six-second steady comparisons
  after one-second warmup. Every cold 100-request burst split 50/50 and made
  exactly two backend fills with 49 local followers per instance. Rejoining an
  empty-cache instance made one new fill; total backend calls became three.
- **Findings:** Final steady median successful throughput was about 611 RPS direct,
  601 through one front target, and 609 through two targets; the backend budget
  stayed fixed, so scaling did not double capacity. One generator drop and one
  failed readiness probe (59 front-level 503s) were retained and reconciled
  separately from gateway admission failures. Probe failure cause is unknown.
- **Corrections/decisions:** An initial 64-connection front pool masked admission
  with another queue and produced generator drops. Retained those diagnostic
  records, increased the per-target ceiling to the generator's 256-client bound,
  and reran final cases. Updated analysis to report generator drops and distinguish
  no-healthy-target responses from downstream admission rejections. A test-only
  stale loop-variable reference was corrected before final checks passed.
- **Verification:** Ordinary/race tests, vet, build, formatting, and whitespace
  checks passed. Tests cover held-backend direct/front bursts, routing counts,
  dead-target 502 without retry, no-target readiness, probe deadlines, input
  validation, and cancellation independent of readiness. All 18 final trials
  reconciled; configured instance and aggregate backend bounds held, final work
  gauges drained, and no owned children remained.
- **Limits:** Local state still duplicates work across processes. Health checks
  are best effort, the front adds its own resource/failure costs, and fixed budget
  allocation is not distributed admission enforcement. Short same-host trials do
  not establish production capacity or zero-downtime failover. See
  [V2.3 guide/results](docs/v2/multi-instance.md) and [tradeoffs](docs/tradeoffs.md).

### 2026-11-03 - V2.4 optional shared cache

- **Scope:** Shared response storage without distributed leases.
- **Changes:** Added pinned go-redis v9.7.3 adapter, small Store interface,
  local/shared/layered CLI modes, tier/error metrics, bounded pool and operation
  deadlines, versioned SHA-256 key namespace and safe bounded JSON responses.
  Atomic STRLEN/GET/PTTL reads subtract lookup time; PXAT and original expiry
  preserve freshness through publication and repeated L1 promotion.
- **Decisions:** Redis is optional; failures fall back to local flight/limiter.
  Readiness stays local. Safe shared replay omits arbitrary extension headers.
  Publication remains unconditional; cold misses may duplicate across instances.
- **Verification:** Real Redis 7.4.2 built under WSL2, 64 MiB allkeys-lru,
  persistence disabled. Nine final fresh-process trials reconciled 102 successes
  each: local mode two backend fills; shared/layered one. Real integration tests
  cover promotion expiry, safety, corrupt/oversized/persistent data and duplicate
  cold fills. Ordinary/race suites, vet and build passed; dependency tests cover
  slow/unavailable servers, cancellation, capped connections and error metrics.
- **Corrections:** Windows-mounted Redis compilation was slow, so rebuilt on the
  native WSL temporary filesystem. Corrected shell line endings and the benchmark
  client's Connection: close header, which properly triggered cache bypass. Kept
  the failed client trial and superseded successful trials as diagnostic artifacts.
- **Evidence/limits:** [V2.4 report](docs/v2/shared-cache.md) and final artifacts
  `bench/results/v2/20261103-214917-v24`. Short sequential latency is not capacity
  evidence. Absolute timestamps assume aligned clocks; leases and expanded
  dependency fault/degradation studies remain V2.5/V2.6 work.

### 2026-11-03 - V2.5 bounded cross-instance leases

- **Scope:** Optional token-owned Redis fill coordination after local coalescing.
- **Changes:** Atomic lease acquisition, ownership-checked publication/release,
  recheck after acquisition, shorter owner deadline and bounded jittered polling.
  Rejected/ambiguous publication never warms L1 and returns an observable 503.
  Added CLI timing validation, lease counters/gauges/wait timing and rejection logs.
  Unshareable responses retain individual streaming fallback under the limiter.
- **Decisions:** Leases are opt-in and require shared storage/local coalescing.
  Coordination errors fail closed on misses while fresh L1 hits remain available.
  No renewal, distributed strict limiter or exactly-once guarantee. Lease trials
  use 64 MiB noeviction, avoiding ordinary data eviction of live leases.
- **Verification:** Real Redis 7.4.2 tests cover concurrent ownership, expiry,
  stale publication/release with response versions, 100-client two-gateway sharing,
  rejected-result L1 safety, cancellation isolation, timeout cleanup, fallback,
  post-acquisition recheck and fresh L1 dependency isolation. Complete ordinary
  and race suites, vet and build passed; final three added cases passed focused
  ordinary/race checks. Nine separate-process trials reconciled: shared-only
  bursts made two backend attempts; leased bursts made one. Killing an owner
  yielded an expected client transport error and survivor success in 1.48-1.50s,
  including the 500ms backend delay. Survivor work gauges drained.
- **Correction:** Final review classified wait expiry during lease acquisition
  as coordination_timeout, matching the polling-timeout path. Focused ordinary
  and race tests cover this boundary; the successful process paths were unchanged.
- **Evidence:** [V2.5 report](docs/v2/leases.md), raw trials and hashes under
  `bench/results/v2/20261103-220609-v25`, and v25 test/race logs.
- **Limits:** Single Redis authority, finite leases and bounded waiting do not
  bound all followers or prevent duplicate execution after expiry/failure. All
  participating response writers must use the same lease policy. Broader Redis
  faults and configurable degraded-mode policy remain V2.6.

### 2026-11-03 - V2.6 fault injection and degraded modes

- **Scope:** Explicit lease degradation policy, bounded cache admission and
  controlled dependency failure/recovery verification.
- **Changes:** Added protection-first (default) / available-local policies,
  local-only fallback publication, ambiguous-result delivery without L1 admission,
  fallback headers/counters/bounded logs, and separate synchronous flight/client
  caps (CLI defaults 64/1024). Cache hits bypass admission; canceled clients release
  slots while shared work retains its flight reservation until completion.
- **Verification:** Ten final isolated real Redis cases passed with race-instrumented
  gateway binaries: outage, 350ms delay, OOM, Redis+gateway restart and corrupt data
  for both policies. Cold fault bursts returned 100 x 503 with zero backend work
  under protection-first; available-local returned 100 x 200 with two local fills.
  Fresh L1 hits stayed available. Recovery probes retain errors and prove renewed
  shared reuse; all final work gauges drained and lease keys were absent.
  Complete ordinary/race suites, vet and build passed. Real Redis tests cover
  ambiguous committed replies, stale owners, expiry and rejected local admission;
  focused tests cover caps, cancellation, timeout/permit cleanup and safe logs.
  Current-code V2.5 process regression rechecked one leased fill and killed-owner
  recovery with the expected interrupted-client error retained.
- **Corrections:** Initial diagnostic runs exposed a transient WSL forwarding EOF
  and an intentionally unretried stale Redis connection. Hardened harness control
  readiness/shutdown, retained first-operation failures and recorded bounded
  external recovery probes; application command/backend retries remain disabled.
- **Evidence:** [V2.6 report](docs/v2/faults.md), final matrix
  `bench/results/v2/20261103-223343-v26`, process regression
  `bench/results/v2/20261103-223608-v25`, and v26 ordinary/race logs.
- **Tradeoffs/limits:** Available-local can duplicate work, admit summed local
  capacity and retain divergent L1 copies. Protection-first trades miss availability
  for backend protection. Admission bounds cover cache flights/waiting clients,
  not all connections/RSS. OOM fixture is deterministic rejection, not a memory
  benchmark. Final comparisons/soak remain V2.7; original V1 plan/results unchanged.

### 2026-11-03 - V2.7 final comparison, soak and reproduction

- **Scope:** Final four-mode evidence and explanation; complete the V2 plan.
- **Changes:** Added a standard-library-only cross-platform process sampler,
  rotating-order four-mode harness, reconciliation/summary tooling, resumable
  experiments with application/binary checks and attempt-specific files, one-command
  ordinary/race/vet/build/fault/source-only verification. Generator reports all
  coordination/cache admission/degraded reasons. Added graceful-drain and reason
  accounting tests. Published final topology, configuration, trial ranges,
  per-process resources, failure boundaries and measured project-story claims.
- **Evidence:** `bench/results/v2/20261103-224701-v27`: twelve 30-second/5-second-warmup
  mixed trials (three per mode), twelve 100-client cold bursts, one 300-second soak.
  Four modes share 500 offered RPS, front hop and aggregate 32 backend permits/queue
  slots. Cold fills were 1/2/2/1 for local-one/local-two/shared/leased. All 25 trials
  reconciled. Principal results retain one generator drop and one coordination 503.
  Soak: 150,000 sent, 149,997 successful, three coordination 503s, no drops/transport
  errors. Gateway first/last-60s RSS medians were 18.75/20.44 and 18.72/19.65 MiB;
  generator max sampled RSS 81.54 MiB includes retained latency samples.
- **Verification:** Complete real-Redis ordinary/race suites, vet and build passed,
  including graceful draining/forced shutdown. Ten final fault cases passed under
  `20261103-230623-v26`. A source-only snapshot rebuilt all Go programs and passed
  eight smoke cases; source hashes match and no prebuilt Go binaries were copied.
  Redis and all owned child processes were cleaned up by the harness.
- **Correction:** Readiness returned an invalid echoed HTTP status line before
  shared cold trial 3; exact cause unproven. Added bounded HTTP parsing retry and
  distinct fixture ports, then resumed without repeating completed trials. Original
  FAILED/logs and initial manifest remain; resume-1.json records unchanged app/
  binaries and updated harness hashes. Final SUCCESS marks completed checks.
- **Decisions/limits:** Fixed offered-rate throughput is not peak capacity; adding
  Redis/leases can add cost without much steady-work savings. Redis test build is
  libc/-O1 in WSL2; process RSS excludes VM/kernel overhead. No leak-free, production
  reliability, clean Git commit, strict distributed limit or exactly-once claim.
  Historical V1/earlier V2 evidence and the original V1 plan were preserved.
- **Report:** [Final comparison and reproduction](docs/v2/final.md),
  [process resources](docs/v2/final-resources.md), and [tradeoffs](docs/tradeoffs.md).

### 2026-11-03 - V3.0 frozen baseline and configurable harness

- **Scope:** First V3 milestone from the
  [V3 plan](high_performance_api_gateway_v3_plan.md). Freeze V2, generalize the
  runner, attribute dependency errors, and add cost-per-fill columns. No caching,
  coalescing, admission or lease behavior changed.
- **Changes:**
  - Tagged `66c9ccd` as `v2-final`.
  - Added `bench/run-v3.py`. Its defaults reproduce V2.7. It parameterizes
    modes, workload, a rate list, gateway count (2/4/8), steady and cold backend
    delay, backend max-age and generator concurrency. It reuses the frozen V2.7
    build, verification and identity code, plus its resume and clean-source flow.
  - Added `bench/summarize-v3.py`, which keeps every V2.7 reconciliation check
    and adds ops per fill, ops per request, backend calls per request and
    per-trial error causes.
  - `run-v26.py` gained `--results`, with an unchanged default, so V3 fault
    runs stay inside V3 artifacts.
  - The backend gained `-max-age` (default 10).
  - Four fixed counters split `dependency_errors` into timeout, pool_timeout,
    connection and other.
  - The Redis adapter now reserves one slot per pool connection inside the
    existing operation deadline, so a saturated pool reports `ErrPoolWait`.
- **Evidence:**
  - `bench/results/v3/20261104-021358-v3` ran one 30-second V2.7-settings block
    per mode. Backend calls were 2,973/2,976/2,976/2,973. Dependency operations
    were 5,952 and 14,880, which is 2.00 and 5.01 per fill. Cold fills were 1/2/2/1.
    Every count was inside V2.7's trial ranges. There were no drops, 503s,
    transport errors or dependency errors.
  - Latency did not reproduce. local-one ran first after the build, at p95/p99
    65.66/153.80 ms with 939 late arrivals; the cause was not established.
    The other modes were below their V2.7 ranges. With a single trial, neither
    is an effect.
- **Verification:**
  - `20261104-020914-v3` (`--quick --complete`) passed: real-Redis ordinary and
    race suites, empty vet/build logs, all ten fault cases, and a source-only
    rebuild with eight smoke trials.
  - In those fault runs, causes matched each fault and summed to the totals:
    outage 12/2 connection, `CLIENT PAUSE` 2/2 timeout, OOM 16/2 other,
    restart 1/1 connection, corrupt value 0.
  - Before the harness runs, local `gofmt`, `go build ./...`, `go vet ./...` and
    `go test ./...` passed.
- **Decisions/limits:**
  - go-redis v9.7.3 keeps its pool-timeout sentinel internal, and its 50 ms pool
    wait equals the operation deadline, which starts first. Hence the adapter
    slots rather than matching error text. The plan's claim that go-redis exports
    `ErrPoolTimeout` was corrected.
  - The lease-protocol option waits for V3.2. `local-two` keeps its V2.7 name
    for any gateway count.
  - `92a0ad7` keeps its `\` message; rewording it would rewrite the commit both
    branches share.
  - V3.0 work is uncommitted. V1/V2 artifacts are unchanged.
- **Report:** [V3.0 harness, counters and acceptance evidence](docs/v3/README.md).

### 2026-11-03 - V3.1 noise floor and V3 baseline

- **Scope:** Second V3 milestone from the
  [V3 plan](high_performance_api_gateway_v3_plan.md): measure the noise floor,
  reduce host noise, and rerun the four-mode comparison as the V3 baseline. No
  Go code changed. Caching, coalescing, admission and lease behavior are the
  same as in V3.0.
- **Changes:**
  - Added `bench/host.py`, with no dependencies:
    - reads physical cores and efficiency classes
      (`GetLogicalProcessorInformationEx` on Windows, sysfs on Linux);
    - resolves `--affinity off|auto|explicit sets`;
    - pins processes (`SetProcessAffinityMask` after start on Windows,
      `sched_setaffinity` before `exec` on Linux);
    - records the power plan, the Windows 11 power-mode overlay and AC state.
  - `run-v3.py`:
    - New `local-two-aa` mode: a second copy of `local-two` in every block,
      included by default. It is refused without `local-two`.
    - `--trials` now defaults to 5. With five modes, the rotation puts every
      mode once in every position.
    - New `--affinity` option. Its default became `off` after the pinned runs
      below. When pinning, the V3 topology launches its own processes: the
      gateways, the generator, and backend plus front proxy each get a CPU set
      and `GOMAXPROCS` equal to its size, and every mask is read back and
      asserted.
    - The manifest records the CPU sets and power state, and so does every
      trial.
    - Resume requires the same resolved CPU sets.
    - The clean-source check passes `--affinity` through.
  - `summarize-v3.py`:
    - Paired per-block differences against `local-two` for send p50/p95/p99,
      scheduled p99 and backend calls, with positive-block counts.
    - The A/A noise floor: the largest |`local-two-aa` − `local-two`| per metric
      and rate. A median difference beyond it is marked `*`.
    - Reports the CPU sets and counts trials not on AC power. V3.0 artifacts
      still summarize, printing a no-floor warning.
  - Docs:
    - Added `docs/v3/noise-floor.md`. Its predictions were written before any
      measured run.
    - Updated the V3 README options, the bench README, the results index, the
      plan status and outcome, and the project README.
- **Evidence:** six runs, all `SUCCESS`, all on AC with the Balanced plan and
  the balanced overlay.
  - `20261104-043055-v3` (`--quick`) was a functional check. Every process
    recorded its read-back CPU set and `GOMAXPROCS`.
  - Pinning made noise worse. These are unfavorable results, and they are kept:

    | Run | Affinity | p95 / p99 floor | Late arrivals per trial | Median p50 |
    |---|---|---|---|---|
    | `043318`, 5-mode baseline, `--complete` | auto | 61.75 / 373.24 ms | 409–1,875 | 1.45 ms |
    | `045906`, A/A only | off | 1.02 / 11.81 ms | 46–351 | 0.66 ms |
    | `050522`, A/A only | auto | 6.73 / 87.69 ms | 340–1,056 | 1.19 ms |
    | `053815`, A/A only | backend and front off CPU 0's core | 1.14 / 53.01 ms | 499–724 | 1.26 ms |

    - The first pinned baseline had front-proxy 503s in 3 trials (45, 23, 82)
      as 250 ms readiness probes timed out. Its leased soak had 27 503s from 8
      Redis errors, all classified `timeout`.
    - Moving backend and front off CPU 0's core removed the 503s and the p95
      spread. Late arrivals, p50 and the p99 floor stayed high, so the cause is
      only partly isolated.
  - The **V3 baseline** is `20261104-051315-v3`: `--complete`, unpinned, 5
    modes × 5 blocks.
    - The noise floor was p50 0.14, p95 0.40 and p99 2.63 ms; scheduled p99
      4.02 ms; backend calls 0.
    - Paired against local-two: shared p95 **+1.93\*** ms (5/5 blocks) and
      p99 +3.37\*; leased p95 **+4.39\*** ms (5/5) and p99 +5.86\*; local-one
      p95 −0.06 ms, within noise.
    - Backend calls: leased and local-one −3\* per block, shared 0.
    - Ops per fill were 2.00 / 5.01. Blocks 1–3, which share V2.7's seeds,
      match V2.7's backend calls exactly (leased block 3 is off by 1).
    - Cold fills were 1/2/2/1/2 in each of 3 trials.
    - Leased block 4 had one host spike: p99 161 ms, 495 late arrivals, and 2
      coordination 503s from 2 Redis `timeout` errors.
    - The soak: 150,000/150,000 successes, 0 dependency errors, p99 59.66 ms,
      5.005 ops per fill. V2.7's soak had 3 coordination 503s.
  - Predictions:
    - Held: the floor range, in the A/A-only run; the paired p95 ranges for
      leased and shared; counts unaffected.
    - Partly failed: shared also exceeded the p99 floor, by 0.7 ms.
    - **Failed:** the prediction that pinning would lower noise.
- **Verification:**
  - The baseline's `--complete` passed:
    - 10/10 ordinary and 10/10 race test packages against harness-owned Redis;
    - empty vet and build logs;
    - all ten fault cases (`faults/20261104-053621-v26`);
    - the source-only rebuild.
  - Locally, `go build ./...` and `go vet ./...` passed, and `gofmt -l` was
    empty.
  - Argument checks reject overlapping CPU sets and an A/A copy without
    `local-two`.
- **Decisions/limits:**
  - `--affinity` defaults to `off`; `auto` and explicit sets remain for other
    hosts.
  - The floor is the largest of five A/A differences. It is a conservative,
    crude bound, not a confidence interval.
  - The floor differs between runs (0.40 against 1.02 ms at p95 unpinned), so
    every run carries its own A/A copy.
  - The A/A copy was slightly slower in all 5 blocks (0.03–0.40 ms), even with
    rotation. There may be a small systematic offset; its cause is unknown.
  - The pinned-against-unpinned comparison ran sequentially, but the unpinned
    run sat between two pinned ones.
  - Redis inside WSL2 and the harness are never pinned.
  - V3.0 is committed as `ac24ab8`. A test re-summary of its artifact
    `20261104-021358-v3` rewrote that `summary.json`, so it was restored from
    git.
  - The V3.1 work is uncommitted.
- **Report:** [V3.1 noise floor and baseline](docs/v3/noise-floor.md).

### 2026-11-04 - Pack V3 run logs into one archive per run

- **Scope:** The six V3.1 run folders held 1,092 files, mostly per-process
  logs: 6–8 per trial, plus fault and clean-source copies. Reduce the count
  without losing anything the summaries or a later investigation need.
- **Changes:**
  - Added `bench/pack_logs.py`. It moves every `*.log` and `*.conf` under a
    run folder, including `faults/` and `clean-source-results/`, into that
    folder's `logs.zip`.
    - It appends on repeat packing, as after a resume, and renames clashing
      names `~N`.
    - It CRC-checks the archive and compares sizes before deleting the
      originals.
    - It also runs standalone: `python bench/pack_logs.py <run>`.
  - `run-v3.py` packs at the end of every run. On success it packs before
    writing `SUCCESS`. On failure it packs after `FAILED`, and a packing
    error leaves the logs in place without hiding the original error.
  - `manifest.json`, `trials.jsonl`, `*-resources.jsonl`, `summary.json` and
    the check outputs stay as files.
  - Documented the folder layout in `bench/results/v3/README.md` and the
    bench README.
- **Evidence:**
  - Scratch test: a nested `faults/` log was archived with its relative path;
    a second pack appended a clashing name as `~2`; a third pack was a no-op;
    data files were left alone.
  - `bench/results/v3/20261104-070718-v3` (`--quick --skip-build`) passed and
    packed its own logs: 70 files became 14, with 56 in `logs.zip`.
  - The six V3.1 folders were packed afterwards: 1,092 files became 198, and
    about 23 MB became 20 MB.
  - Each folder was re-summarized from a copy of its remaining files, and every
    `summary.json` matched exactly.
- **Decisions/limits:**
  - Resource samples stay unpacked. The summarizer reads them, and they are
    most of the bytes.
  - The committed V3.0 folders and the V2 folders keep their loose logs as
    recorded.
  - No docs linked to individual logs, so no links changed.
  - This work is uncommitted, along with V3.1.
- **Report:** [V3 experiment records](bench/results/v3/README.md).

### 2026-11-04 - Resume bullet candidates

- **Scope:** Draft resume bullets for the whole project, V1 through V3.1, for
  choosing the top three.
- **Changes:** Added `resume_bullets.md`. It has 15 bullets in three groups:
  core gateway, distributed caching and reliability, and measurement. It also
  suggests a top three and lists what to be ready to explain in an interview.
- **Evidence:** Every number links to its recorded source: `docs/results.md`,
  `docs/v2/`, `docs/v3/` or the V3.1 baseline logs. All link anchors were
  checked to exist.
- **Decisions/limits:**
  - Only measured results are used. The V3.2 lease fix is described as
    designed, not built.
  - V1 numbers carry their single-process, single-laptop caveat.
  - Update the file when V3.2 and later results are measured.
- **Report:** `resume_bullets.md` (local only; ignored by git).

### 2026-11-04 - Resume evidence review

- Reviewed candidate claims against V1 results and V2/V3.1 reports.
- Selected three final bullets covering performance, distributed coordination,
  and reliability tooling in `resume_bullets.md`.
- Removed unsupported steady-memory wording and brittle test counts; separated
  historical V1 measurements from later admission changes and proposed work.
- Qualified latency comparisons and limited timeout attribution to V3.1.
- Documentation-only review; no application changes or benchmark reruns.

### 2026-11-04 - V3.2 lease protocol v2

- **Scope:** Third V3 milestone from the
  [V3 plan](high_performance_api_gateway_v3_plan.md): cut leased mode's
  coordination round trips, keep v1 selectable in the same binary, and compare
  both protocols in the same blocks.
- **Changes:**
  - `internal/sharedcache/lease.go` adds `ReadOrAcquire`, a single Lua script
    that keeps the lookup script's size and PTTL checks. It returns hit (value
    and PTTL), owner (`SET NX PX` with a fresh 256-bit token) or wait.
    - The script sits behind a new `AtomicCoordinator` interface, so v1 code and
      fakes are unchanged.
    - An undecodable value is reported unusable. The caller then claims again
      with the read skipped, so an owner overwrites it.
    - `newToken` is shared with v1's `Acquire`.
  - `internal/gateway/lease.go`:
    - One `claim` step per loop iteration: v1 reads then acquires, v2 runs the
      script.
    - v2 has no post-acquire recheck and skips the release only after an
      acknowledged publish. Every other exit still releases by token.
    - Followers poll with the same script.
    - The lease start time is taken at the acquiring operation, so v1's
      deadline is exactly as before.
  - `-lease-protocol v1|v2` gateway flag and `LeaseOptions.Protocol`. v2
    requires an `AtomicCoordinator`. After acceptance, the CLI default became
    `v2`; the library zero value still means v1.
  - Tests:
    - 12 existing lease, degradation and publication tests now run under both
      protocols (`forProtocols`).
    - New: `claim_test.go` (atomic hit-or-acquire, unusable and oversize values,
      stale v2 owner) and `lease_v2_test.go` (5 against 2 operations per fill,
      corrupt value refilled without stall, recovery after an abandoned lease,
      protocol validation).
  - Harness:
    - `run-v3.py` adds a `leased-v2` mode; `leased` passes v1 explicitly.
    - `--complete` builds `bin/gateway-race` and runs the ten fault cases under
      v1, v2 and v2 race. `--soak-mode` now defaults to `leased-v2`.
    - `run-v26.py` gains `--lease-protocol`, defaulting to v1.
    - `summarize-v3.py` adds Redis commands per fill (`total_commands_processed`
      in the sampled window, net of the sampler's `INFO` calls), same-block
      protocol contrasts and a rejection total.
  - Docs: added `docs/v3/lease-protocol.md`, with predictions written before
    the run. Updated the plan status, outcome and resume bullet, the README,
    the V2 lease flag table, the V3 README, the bench README and the results
    index.
- **Evidence:** acceptance run `bench/results/v3/20261104-153628-v3` (`--complete
  --trials 6 --soak-mode leased-v2`, 6 modes × 6 blocks), `SUCCESS`.
  - Ops per fill: shared 2.00, leased 5.00, leased-v2 2.00.
  - Redis commands per fill: shared 3.00, leased 12.01, leased-v2 8.01 (−33%).
  - Same-block contrasts, against a floor of 0.72 ms p95 and 2.03 ms p99:
    - leased-v2 − leased p95 **−2.11\*** ms [−2.95, −1.25], 0 of 6 blocks
      slower; p99 **−3.77\*** ms;
    - leased-v2 − shared p95 +0.07 ms, within the floor;
    - leased − shared p95 +1.96\* ms.
  - Backend work was identical to leased in every block (3 fewer than
    local-two), with 3 distributed followers per block and 1 fill per cold
    burst.
  - All 36 steady trials had 0 503s and 0 dependency errors.
  - leased-v2 soak: 150,000/150,000 successes, 0 errors, p99 55.44 ms, 2.003
    ops per fill, gateway RSS 18.2 → 19.6–19.8 MiB.
  - Predictions: 6 held. The p95 gain (−2.11 ms) fell below the predicted −2.5
    to −3.5 ms, but closed the whole v1 gap to shared in this run (1.96 ms).
    The plan's original "−60% Redis commands" was replaced by −33% before the
    run, because V3.1's data showed Redis counts commands inside scripts. The
    measured drop was −33.3%.
- **Verification:**
  - Before the run: `go build`, `go vet` and `gofmt -l` were clean. Ordinary
    and race suites passed against a temporary Redis 7.4.2, with no skipped
    real-Redis tests.
  - The acceptance run's own checks: 10/10 ordinary and race packages; empty
    vet, build and race-build logs; all ten fault cases in `faults/v1`,
    `faults/v2` and `faults/v2-race`, with the same error causes per fault
    under both protocols; and the source-only rebuild.
  - Functional check `20261104-153406-v3` (`--quick`) passed.
  - After the default switch, `go test ./cmd/gateway` passed.
- **Decisions/limits:**
  - The default switched to v2 after the acceptance run. That run's binaries
    had the v1 default, but every mode passed its protocol explicitly.
  - Like v1, v2 is one Redis authority with expiring tokens, not fencing. The
    script touches two keys, so Redis Cluster would need a shared hash slot;
    Cluster is out of scope.
  - `mixed` still has no cross-node reuse, so V3.2 removed lease cost without
    adding benefit; that is V3.3's job.
  - Go files must stay LF (`.gitattributes`). Two files briefly became CRLF
    through a Python edit and were restored before testing.
  - Work is uncommitted.
- **Report:** [V3.2 lease protocol v2](docs/v3/lease-protocol.md).

## Future entry template

### YYYY-MM-DD — Change title

- **Scope:** Requested task or milestone.
- **Changes:** Files and resulting behavior.
- **Decisions:** Tradeoffs and relevant constraints.
- **Verification:** Commands actually run and their outcomes.
- **Outstanding:** Failures, limitations, or work still required.
