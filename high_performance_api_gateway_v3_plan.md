# API Gateway v3 — Turning V2's mechanisms into measured results

**Status:** V3.0 implemented: [harness, error causes and derived costs](docs/v3/README.md).
V3.1 implemented: [noise floor and V3 baseline](docs/v3/noise-floor.md).
V3.2 implemented: [lease protocol v2](docs/v3/lease-protocol.md).
V3.3 implemented: [reuse workloads, predictions and restart](docs/v3/reuse-workloads.md).
V3.4–V3.6 planned.

**Baseline:** V2.7 final study [`20261103-224701-v27`](bench/results/v2/20261103-224701-v27/summary.json),
reported in [docs/v2/final.md](docs/v2/final.md). V1 and V2 results stay unchanged.
Write V3 artifacts under `bench/results/v3/` and reports under `docs/v3/`.

**Purpose:** V2 showed that the shared cache and leases are correct and bounded.
It did not show a steady-state benefit from either one. V3 does three things:
1. Removes avoidable coordination cost.
2. Tests the workloads these mechanisms are designed for.
3. Measures capacity, instead of a fixed arrival rate that every mode already meets.

## 1. Why V2.7 showed no steady-state benefit

| Observation in V2.7                                                                                             | Cause found in code or artifacts                                                                                                                                                                                                                                                                                                                         | Consequence                                                                                          |
| --------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| Leased mode made 14,882 Redis operations for 2,973 backend fills (5.0 per fill). Shared mode made 2.0 per fill. | The lease owner runs five operations in sequence: read, `SET NX`, recheck read, publish and release ([lease.go](internal/gateway/lease.go)). The publish script already deletes the lease ([sharedcache/lease.go](internal/sharedcache/lease.go)), so the release never deletes anything. One leased gateway recorded 1,722 publications and 0 releases. | Every owner miss pays 3 extra Redis round trips before clients get a response.                       |
| Leased send p95 was about 4 ms above shared and 5.6 ms above local-one.                                         | The mean Redis operation took about 1.0 ms through WSL2 (`cache_dependency`: 8.82 s over 8,620 operations).                                                                                                                                                                                                                                              | 3 extra round trips cost about 3 ms, which may explain most of the gap. This is an estimate to test. |
| The shared lookup hit ratio was 0.00%.                                                                          | The generator has one hot key (`id=42`), which stays in both nodes' L1. Unique keys never repeat ([loadgen](cmd/loadgen/main.go#L72-L76)).                                                                                                                                                                                                               | The workload has no cross-node reuse for shared storage to provide.                                  |
| Leases saved about 3 fills per trial.                                                                           | With one hot key and the backend's fixed `max-age=10` ([handler.go:76](internal/mockbackend/handler.go#L76)), there is one synchronized expiry every 10 s, so 3 per 30 s trial.                                                                                                                                                                          | Duplicate suppression had only 3 chances per trial.                                                  |
| Every mode delivered about 499 successful RPS.                                                                  | The offered rate was fixed at 500/s. Backend miss capacity is 32 permits ÷ 50 ms = 640/s, and about 20% of requests miss.                                                                                                                                                                                                                                | Throughput cannot differ until offered load approaches a bottleneck.                                 |
| The p99 ranges overlap across modes.                                                                            | There were 3 trials on one shared host, with no measured noise floor.                                                                                                                                                                                                                                                                                    | Small latency effects cannot be told apart from noise.                                               |

## 2. Central question

> At the same backend budget, how much backend work and capacity do shared
> storage and leases save on workloads with cross-node reuse? What do they cost
> on workloads without it?

## 3. Rules that keep improved results honest

- Write each milestone's predictions into its report before running it.
- Keep V2.7's `mixed` workload in every comparison as the no-reuse control.
  Report any workload chosen to show a benefit next to this control.
- Keep the aggregate backend budget equal across modes (permits, queue slots and
  cache admission), as V2.7 did.
- When a protocol changes, keep the old one selectable in the same binary so A/B runs share the same blocks.
- Do not tune lease or Redis timeouts per workload without reporting it.
  Raising a timeout to hide errors is not a fix.
- Lead with backend calls and capacity at an SLO. They depend far less on host
  noise than p99 does on a shared laptop.
- Keep failed and unfavorable runs, as V2 did.

## 4. Milestones

### V3.0 — Freeze V2 and generalize the harness

**Work**

- Commit the uncommitted V2.7 work and tag it `v2-final`. If the branch has not
  been pushed, consider rewording commit `92a0ad7`, whose message is `\`.
- Create `bench/run-v3.py` from [run-v27.py](bench/run-v27.py). Turn its fixed
  choices into parameters: modes, workload, rate list, backend delay and
  max-age, and gateway count. The lease-protocol parameter arrives with V3.2,
  when a second protocol exists. Keep reusing the [run-v26.py](bench/run-v26.py) topology.
- Add fixed counters for dependency-error causes: `dependency_error_timeout`,
  `dependency_error_pool_timeout`, `dependency_error_connection` and
  `dependency_error_other`. go-redis v9.7.3 keeps its pool-timeout sentinel
  internal, and its 50 ms pool wait equals the operation deadline. So the
  adapter reserves connection slots itself and reports `ErrPoolWait` when the
  deadline expires first. Timeouts appear as `net.Error.Timeout()` or
  `context.DeadlineExceeded`. Never put raw error text in labels. V2.7 could
  not say why its coordination 503s happened.
- Add derived summary columns: dependency operations per backend fill,
  dependency operations per client request, and backend calls per client request.

**Acceptance**

- `run-v3.py --quick`, run with V2.7 settings, reproduces V2.7's counts: backend
  calls within trial ranges, and 2.0 and 5.0 operations per fill.
- In tests, the error-cause counters sum to `dependency_errors`.

### V3.1 — Measure and lower the noise floor

**Work**

- Run five trials per mode instead of three. Keep the block rotation and shared seeds.
- Add an A/A control: run `local-two` twice in every block. The spread between
  the two copies is the noise floor. Print it beside every latency table, and do
  not call any difference below it an effect.
- Report paired differences per block against `local-two` (same seed, same
  block), in addition to per-mode medians.
- Give each native process its own logical-CPU set (generator; gateways; backend
  and front). Use `SetProcessAffinityMask` through `ctypes` on Windows and
  `os.sched_setaffinity` on Linux. Affinity is applied after the process starts,
  so also set `GOMAXPROCS` to the size of each set. Record the masks. Redis
  inside WSL2 cannot be pinned this way; record that limitation.
- Record the power plan and AC power state in the manifest.

**Outcome:** Pinning made the noise floor larger on this hybrid laptop, so it
is opt-in (`--affinity auto`). The unpinned baseline's floor is 0.40 ms at p95
and 2.63 ms at p99. Paired p95 is +1.93 ms for shared and +4.39 ms for leased,
which puts the leased−shared gap that V3.2 targets at about 2.5 ms.

**Acceptance**

- The noise floor for send p95/p99 is documented.
- The V2.7 four-mode comparison, rerun under the V3 harness, becomes the new
  baseline that later milestones compare against.

### V3.2 — Cut coordination round trips (lease protocol v2)

**Work**

1. **Atomic read-or-acquire.** Write one Lua script over the response key and
   the lease key. It returns one of three answers:
   - `hit`: the value and its PTTL, with the same size and TTL checks as the
     current lookup script.
   - `owner`: `SET NX PX` succeeded.
   - `wait`.

   This replaces the initial read, the `SET NX` and the owner's recheck. Because
   the script is atomic, the race that the recheck existed to close cannot happen.
2. **No release after an acknowledged publish.** The publish script already
   deletes the lease. Keep the token-checked release on every other exit path:
   fill error, unshareable response, publish error and lost lease.
3. **Followers poll with the same script**, so each poll costs 1 operation instead of 2.
4. **Corrupt shared values must not stall misses.** If the script reports a hit
   but decoding fails, fall back to acquiring the lease so the owner overwrites
   the bad value. Without this, waiters would poll until the coordination timeout.
5. Select the protocol with `-lease-protocol v1|v2`. Keep `v1` as the default
   until V3.2 is accepted.

**Tests**

- Against real Redis: atomic hit-or-acquire; stale owner still rejected; release
  skipped only after an acknowledged publish; a corrupt value is refilled by an
  owner without a stall; recovery after lease expiry.
- All ten V2.6 fault cases under protocol v2, with ordinary and race builds.

**Prediction**

- Operations per fill drop from 5.0 to 2.0, matching shared mode.
- Redis `total_commands_processed` per leased trial drops by about 60%.
- The owner miss path loses 3 synchronous operations. At V2.7's mean of about
  1.0 ms per operation, leased send p95 on `mixed` should land within the
  noise floor of shared mode.

**Acceptance**

- Rerun the four-mode `mixed` comparison with both protocols in the same blocks.
  Report operations per fill, paired p95/p99 differences, and 503 counts with causes.
- If the p95 gap does not shrink as predicted, report that. Find the remaining
  cost (polling, pool waits, scheduling) before starting V3.3.

**Outcome:** Accepted, and `-lease-protocol` now defaults to `v2`.
- Operations per fill fell from 5.00 to 2.00, the same as shared.
- Redis commands per fill fell from 12.01 to 8.01 (−33%). The ~60% above
  assumed one command per operation, but Redis also counts the commands a
  script runs.
- In the same blocks, leased p95 fell 2.11 ms and p99 3.77 ms, slower in none
  of 6 blocks. Leased-v2 is within the noise floor of shared (+0.07 ms p95,
  0.72 ms floor).
- There were zero 503s, and all 30 fault runs passed (v1, v2, v2 race).

### V3.3 — Workloads with cross-node reuse

**Work**

- Add two workloads to the generator, seeded like the existing ones:
  - `zipf`: Zipf-distributed keys, with `-keys N -zipf-s S`. Go's
    `rand.NewZipf` requires `s > 1`. Start with N = 10,000 and s = 1.1.
  - `hotset`: K equally likely keys, with `-hot-keys K`. Start with K = 100.
    Pair it with a short backend max-age to test expiry bursts (V2 plan §7,
    "TTL-expiry burst").
- Add a generator `-plan-only` dry run that writes out the seeded key stream.
  `bench/predict-v3.py` then computes ideal fills, ignoring concurrency, for
  three cases: one node, N independent round-robin nodes, and a shared tier.
  Predictions come from the same stream the real run uses.
- Use the backend `-max-age` flag (added in V3.0; the default of 10 keeps V2 behavior).
- Add gateway `-cache-entries` and `-cache-bytes` flags. The defaults of 1000
  entries and 16 MiB keep V2 behavior.
- **Restart scenario.** During a `zipf` run, stop gateway-1 at 15 s and restart
  it with an empty L1 at 20 s. Compare backend calls in the 10 s after restart
  for `local-two`, `shared` and `leased`. Reuse the restart tooling in
  [run-v26.py](bench/run-v26.py).
- **Optional:** run with 4 gateways, splitting the aggregate budgets four ways.
  Report backend calls only; host CPU contention makes the latencies not comparable.

**Prediction** (fill in the numbers from `predict-v3.py` before running)

- `zipf`: shared and leased make fewer backend calls than `local-two`, by about
  the predicted cross-node duplicate fraction. The shared lookup hit ratio is above 0.
- `hotset`: `local-two` makes about 2 fills per key per TTL window. Shared makes
  about 1, plus misses where both nodes request the key at the same moment.
  Leased makes about 1. The overlap fraction is roughly per-key arrival rate ×
  fill time; choose those values so the overlap is not negligible, and state them.
- Restart: with shared or leased, the restarted node serves popular keys from
  Redis, and its backend spike is much smaller than with `local-two`.
- `mixed`: no change from V3.2. No benefit is expected there.

**Acceptance**

- For every workload and mode, report: backend calls per request, shared hit
  ratio, operations per request, success and 503s with causes, and p50/p95/p99
  with the noise floor.
- Report measured fills against predicted fills, and explain any gap
  (simultaneous misses, L1 eviction).

**Outcome:** `predict-v3.py` predicted every workload's backend calls to within
1% per block.
- **zipf:** shared and leased-v2 cut backend calls 18.0% and 18.7% against
  local-two, exactly as predicted, with shared hit ratios of 20.6–20.9%.
- **hotset (1 s TTL):** 35.1% and 41.8%, against 35.7% and 41.9% predicted.
- **mixed:** unchanged.
- **Latency:** the tier costs 0.9–1.7 ms at p95; leases cost nothing beyond
  shared at p95.
- **Restart:** an empty local gateway's first 2 s ran above the steady
  backend rate (323–358 calls against 295). With Redis, they stayed at it.
  Over 10 s, with a 10 s TTL, the effect is small for every mode. Leases use
  the V3.2 protocol (`leased-v2`). The optional 4-gateway run was not done.

### V3.4 — Capacity at a fixed SLO

**Work**

- Define the SLO for a 30 s measured window after warmup:
  - at least 99.9% of responses succeed;
  - scheduled p99 is at most 3× the backend delay;
  - the generator drops no arrivals.
- **Host-ceiling control first.** Run the `hot` workload in `local-two` mode to
  find the highest rate this laptop's generator, front and gateways can drive
  while doing almost no backend work.
- Choose backend capacity (permits ÷ delay) so every predicted mode ceiling is
  at most half the host ceiling. The predicted ceiling is roughly backend
  capacity ÷ backend calls per request. With V2.7's numbers, 640/s ÷ 0.198 ≈
  3,200 req/s for `mixed`, which may be close to the host ceiling. A 100 ms
  backend with 32 permits (320/s) brings every ceiling into a clean range.
- Climb a rate ladder per mode and workload in geometric steps (for example
  ×1.25), then run 3 confirmation trials at the highest passing rate. Report
  the first failing rate and why it failed: permit 503s, coordination 503s,
  generator lag or CPU.

**Prediction**

- `mixed`: all mode ceilings are within noise of each other, because backend
  calls per request are equal.
- `zipf`: the ratio of shared ceiling to `local-two` ceiling is about the
  inverse of their ratio of backend calls per request.

**Acceptance**

- Label every ceiling as backend-bound or host-bound. Do not report host-bound
  ceilings as gateway capacity.
- Keep the aggregate budget equal across modes.

### V3.5 — Optional: key-affinity routing as an alternative to leases

**Work**

- Add front `-routing hash`. A rendezvous hash over the cache-key inputs (path,
  query, `Accept`, `Accept-Encoding`, `User-Agent`) picks the gateway, falling
  back to the next healthy target. Each key then reaches one gateway, so local
  coalescing alone stops cross-node duplicates, with no Redis.
- Compare round-robin with leases against hash routing without Redis on
  `zipf`, `mixed` and the restart scenario.

**Prediction**

- Hash routing matches leased backend calls with zero Redis operations.
- On `mixed`, it sends the single hot key, about 80% of traffic, to one node.
  That imbalance should show up in per-node CPU and latency.
- A node failure moves its keys to peers with cold caches.

This gives direct evidence for the design trade-off: coordination cost versus
load balance and failover behavior.

### V3.6 — Final comparison and write-up

- `docs/v3/final.md` opens with a three-line summary of findings, then the tables.
- Report:
  - the workload × mode × protocol tables;
  - the capacity table;
  - the restart spike;
  - the noise floor;
  - predictions against measurements;
  - every unfavorable result.
- Update the README and resume bullets only from measured V3 numbers. V2.7
  numbers remain historical and are not recalculated.
- One-command reproduction, matching V2.7: `python bench/run-v3.py --complete`
  runs tests, race tests, fault cases and a clean-source rebuild.

## 5. Experiment matrix

| Scenario                   | Modes                       | Primary metric                      | Prediction                                                    | Falsified if                                                                           |
| -------------------------- | --------------------------- | ----------------------------------- | ------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `mixed`, protocol v1 vs v2 | leased (+ shared reference) | Operations per fill; paired p95     | 5.0 → 2.0; leased p95 ≈ shared                                | v2 still well above 2 operations per fill, or the p95 gap is unchanged                 |
| `mixed` control            | all four                    | Backend calls; noise floor          | No mode beats local                                           | A mode wins on backend calls without cross-node reuse (investigate before claiming it) |
| `zipf` steady              | local-two, shared, leased   | Backend calls per request           | Shared/leased below local-two by about the predicted fraction | Shared hit ratio ≈ 0                                                                   |
| `hotset` with short TTL    | local-two, shared, leased   | Fills per key per TTL window        | ≈ 2 / ≈ 1 + overlap / ≈ 1                                     | Leased ≈ shared even though overlap is measured                                        |
| Restart with empty L1      | local-two, shared, leased   | Backend calls in 10 s after restart | Spike much smaller with Redis                                 | No difference                                                                          |
| Capacity ladder            | all, on `mixed` and `zipf`  | Highest rate meeting the SLO        | `zipf` ceiling ratio ≈ inverse backend-call ratio             | Ceilings are host-bound                                                                |
| Hash routing (optional)    | hash vs leased              | Backend calls; per-node balance     | Same calls, no Redis, imbalance on `mixed`                    | —                                                                                      |

## 6. What a good V3 result looks like

These describe the outcome being aimed for. They are not pass thresholds.

- On `mixed`, leased mode costs about 2 operations per fill, and its latency
  cannot be told apart from shared mode. That removes the steady-state lease cost.
- On `zipf`, shared storage and leases cut backend calls against independent
  nodes by a predicted and measured percentage, and capacity at the SLO rises to match.
- After a gateway restart, the backend sees no large spike when the Redis tier is on.
- `mixed` still shows no benefit, and the report explains why.

If a prediction fails, the report says so, and only the claims that held go on the resume.

## 7. Out of scope

- Redis Cluster, failover, Pub/Sub waiter notification, lease renewal, adaptive
  concurrency, Kubernetes and cloud deployment.
- Taking publish off the owner's response path. It changes when local and
  remote callers can see a value; revisit only if V3.2 shows publish latency dominates.
- A Linux bare-metal run would remove most of the ~1 ms WSL2 round trip to Redis.
  If one is done, report it separately rather than mixing it with Windows numbers.

## 8. Order and effort

| Block                       | Milestones | Approximate focused effort |
| --------------------------- | ---------- | -------------------------- |
| Harness and noise floor     | V3.0–V3.1  | 6–10 hours                 |
| Lease protocol v2           | V3.2       | 6–10 hours                 |
| Reuse workloads and restart | V3.3       | 8–14 hours                 |
| Capacity ladder             | V3.4       | 6–10 hours                 |
| Affinity routing (optional) | V3.5       | 4–8 hours                  |
| Write-up                    | V3.6       | 4–6 hours                  |

If time is short, do V3.0 and then V3.2. It is the smallest change with the
clearest predicted effect, and it directly addresses V2.7's main negative result.
V3.3 is the next most valuable, because it gives the shared tier something to do.

## 9. Possible resume bullets after V3

Fill these only from measured results:

- Cut Redis round trips per cache fill from 5 to 2 with an atomic Lua
  read-or-acquire script, removing 2.1 ms from leased p95 (3.8 ms at p99) and
  matching the shared tier within a 0.7 ms noise floor. Measured in V3.2.
- Reduced backend calls by 18.7% (Zipf) and 41.8% (hot set) across 2 gateways
  with a shared Redis tier and leases, within 1% of fills predicted from the
  workload's key stream (V3.3), raising capacity at a p99 SLO from [X] to [Y]
  req/s under the same backend budget (V3.4).
- Kept backend load at its steady rate after a gateway restart by warming the
  empty local cache from Redis. In the first 2 s back, calls were 241–249
  against a steady 242–246, compared with 345 against 295 without Redis (V3.3,
  medians of 3 trials).
