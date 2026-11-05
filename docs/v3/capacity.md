# V3.4 — Capacity at a fixed SLO

Up to V3.3, every comparison ran at one fixed arrival rate (500/s) that every mode
already met. Differences could only show up in backend calls and latency. V3.4
asks how much load each mode can carry before it breaks an SLO, at the same
backend budget. The host-ceiling control runs first, so ceilings set by the
laptop are kept apart from ceilings set by the backend.

## What changed

**Generator** ([cmd/loadgen](../../cmd/loadgen/main.go)): an arrival whose
timer fires after the measured window ends is now sent, like any other late
arrival, and its lag counts in scheduled latency. Before V3.4 these arrivals
were dropped.
- At 3,000/s, a 10 s probe recorded 2 such drops with p99 = 1.5 ms. That alone
  would fail a no-drop SLO.
- A new test, `TestLateFinalArrivalsAreSent`, runs 20,000/s for 100 ms. The old
  generator dropped 3–8 of 2,000 arrivals there; the new one drops none.
- Now only a full in-flight limit (or cancellation) drops an arrival.
- The keys are unchanged: V3.3's predictions recomputed after the change are
  identical.

**Predictor** ([bench/predict-v3.py](../../bench/predict-v3.py)) gains
`--capacity`, a model of each gateway's backend limits:
- **Order:** as in the gateway, a miss first takes a cache-flight slot.
  Followers of a key already in flight do not need one. The miss then reads the
  shared tier or lease, and then waits for an upstream permit.
- **Permits:** with every permit busy, a fill waits in FIFO order, in a bounded
  waiter queue, for at most the 50 ms permit wait. A fill that cannot start is
  rejected (`queue_full` or `wait_timeout`), together with every request
  coalesced onto it.
- **Leases:** a lease follower holds a flight slot on its own gateway while it
  waits. A rejected owner releases its lease, and a later request fills for
  itself.
- **Ignored:** network, Redis and lease-poll time.
- **Ladder:** the model climbs the same rate grid as the harness, with the
  climb trial's seeded stream (seed 43). It reports rejected requests, model
  p99, backend utilization and the predicted ceiling per mode.
- **V3.3 unchanged:** without `--capacity`, the model is exactly V3.3's. All
  five V3.3 runs' recorded predictions recompute identically.

**Harness** ([bench/capacity-v3.py](../../bench/capacity-v3.py),
[bench/summarize-capacity-v3.py](../../bench/summarize-capacity-v3.py)):
- **Reuse:** trials use `run-v3.py`'s topology, budget (split evenly across
  gateways) and reconciliation, unchanged.
- **Grid:** every mode climbs one geometric rate grid, one 35 s trial per rung
  (5 s warmup, 30 s measured). Modes are interleaved in rotating order, so they
  share host conditions.
- **Climb:**
  - a failed rung is retried once, and a second failure ends the climb;
  - the highest passing rung then needs 3 more passing confirmation trials;
  - a failed confirmation moves the candidate one rung down, where the
    confirmations start again.
- **Ceiling:** the highest rung whose climb trial and all confirmations passed.
- **Seeds:** trial numbers set them. Every mode gets the same stream at a given
  rung and trial: climb 43, retry 44, confirmations 45–47.
- **Starting rung:** each mode starts at the highest rung at or below 0.7× its
  predicted ceiling, and steps down if that rung fails twice.
- **Predictions:** `predictions.json` is written before the first trial.
- **Resume:** `--resume` replays recorded trials through the same ladder, so an
  interrupted run continues where it stopped.
- **Labels:** every first failure gets a cause from the failed responses:
  - **backend:** most were permit or cache-admission 503s;
  - **coordination:** most were lease or Redis 503s;
  - **host:** anything else (drops, latency, front or transport errors).

  A ceiling is **backend-bound** only if its first failure was backend-caused
  and the ceiling is at most half the host ceiling. Otherwise it is
  **host-bound**, and it is not reported as gateway capacity.

## SLO

For the 30 s measured window after warmup:

| Check | Bound |
|---|---|
| Responses that succeed (2xx) | at least 99.9% |
| Scheduled p99 (lag + send) | at most 3× the backend delay |
| Generator drops | none (the generator allows 256 requests in flight, its V3 default) |

## Host-ceiling control

`hot` workload, `local-two`, ×1.25 grid. The control does almost no backend work,
so its ceiling is what this laptop's generator, front and gateways can drive.

| Run | Backend | Trials (rate: result, drops) | Ceiling |
|---|---|---|---|
| [`034803`](../../bench/results/v3/20261105-034803-v34/summary.json) | 100 ms | 2000 ✓; 2500 ✗ 321, ✗ 352; 2000 confirmations ✓, ✗ 23 | none |
| [`035219`](../../bench/results/v3/20261105-035219-v34/summary.json) | 1 ms, p99 ≤ 300 ms | 2000 ✓; 2500 ✓; 3125 ✗ 637, ✗ 671; 2500 conf. ✗ 123; 2000 conf. ✗ 8 | none |
| [`035703`](../../bench/results/v3/20261105-035703-v34/summary.json) | 1 ms, p99 ≤ 300 ms | 1000 ✓; 1250 ✗ 43, ✓; 1562 ✓; 1953 ✗ 108, ✓; 2441 ✓; 3052 ✗ 253, ✗ 923 (+63 front 503s); 2441 conf. ✓ ✓ ✗ 56; 1953 conf. ✓ ✗ 160; 1562 conf. ✗ 171; 1250 conf. ✓ ✓ ✓ | **1,250/s** |

- **The first control was confounded.** Its p99 peaked in the same 3 s windows
  in every trial, every 10 s. That is the hot key's max-age. Each expiry starts
  one 100 ms fill, and every request for the key waits on it, so about
  rate × 100 ms requests are in flight at once. At 2,500/s that is 250, at the
  generator's 256-slot limit. Measured that way, the "host ceiling" was
  delay × slots, not the host. The fix:
  - later controls use a 1 ms backend, so the expiry fill costs nothing;
  - they are judged by the 300 ms p99 bound that the planned 100 ms backend
    runs would use (`--slo-p99-ms`).
- **The host does stall.** With the 1 ms backend, the latency spikes are no
  longer periodic: 100–480 ms stalls appear a few times per window in the
  front and gateways.
  - The generator's own timer lag stayed under 5 ms at p99.
  - Total CPU at the ceiling was 1.3 cores of 22 threads.
  - VS Code, open on this repository, used about 1.6 cores while the control
    ran, probably reacting to each trial's new artifact files.
- **Drops depend on rate.** With a 256-request in-flight limit, a stall longer
  than 256 ÷ rate drops arrivals. Between 1,250 and 2,441/s, about one trial
  in three failed on drops, with little dependence on rate. From 3,052/s,
  every trial failed, with hundreds of drops.
- **The strict ceiling is conservative.** The ladder's confirmation rule needs
  four passes at one rung, which only 1,250/s achieved. 2,441/s passed 3 of 4
  trials.
- **At 500/s the stalls are rarer.** No V3.2 or V3.3 trial at 500/s (111
  trials) dropped an arrival.

**Host ceiling: 1,250 req/s, so backend-bound ceilings must be at most 625 req/s.**

## Choosing backend capacity

The plan asks for a backend capacity (permits ÷ delay) that puts every
predicted ceiling at or below half the host ceiling. With the aggregate 32
permits fixed (the V2.7 budget):

| Backend delay | Capacity | Highest predicted ceiling | Within 625/s? |
|---|---|---|---|
| 100 ms | 320 fills/s | 1,519 (zipf shared/leased; mixed local-one) | no |
| 200 ms | 160 fills/s | 690 (mixed local-one) | no |
| **250 ms** | **128 fills/s** | **519 (mixed local-one)** | **yes** |
| 300 ms | 107 fills/s | 429 (mixed local-one) | yes |

The runs use **250 ms**, the fastest backend that satisfies the rule.
- The SLO's p99 bound becomes 750 ms.
- The host control was judged at 300 ms, the bound for the 100 ms backend
  planned at the time. The difference does not matter: every failed host trial
  failed on drops, and no host trial's p99 exceeded 152 ms.
- The permit wait stays at the gateway default of 50 ms.
- Every other budget item is V2.7's.

## Predictions

Written before any backend-bound run, from `predict-v3.py --capacity --delay 250`
on the run grid (×1.1 from 200/s: 200, 220, 242, 266, 293, 322, 354, 390,
429, 472, 519, 571, 628, …).

| Workload | Mode | Predicted ceiling | Predicted first failure | Calls/request at ceiling | Backend utilization at ceiling |
|---|---|---|---|---|---|
| mixed | local-one | 519 | 571 (`wait_timeout`) | 0.198 | 80% |
| mixed | local-two | 429 | 472 (`wait_timeout`) | 0.199 | 67% |
| mixed | shared | 429 | 472 | 0.199 | 67% |
| mixed | leased-v2 | 429 | 472 | 0.199 | 67% |
| zipf | local-one | 390 | 429 (`wait_timeout`) | 0.260 | 79% |
| zipf | local-two | 266 | 293 (`wait_timeout`) | 0.347 | 72% |
| zipf | shared | 322 | 354 (`wait_timeout`) | 0.279 | 70% |
| zipf | leased-v2 | 322 | 354 (`wait_timeout`) | 0.273 | 69% |

What these predict:
- **mixed:** the three two-gateway modes have equal ceilings, because their
  backend calls per request are equal. local-one is predicted two rungs (21%)
  higher. That differs from the plan, which expected every `mixed` ceiling
  within noise. The reason is that one gateway pools all 32 permits and
  32 waiter slots, while two gateways split them 16 and 16. Fills arrive at
  random, so two queues of 16 overflow at lower utilization than one queue
  of 32 (67% against 80%).
- **zipf:**
  - shared and leased-v2 are predicted two rungs (21%) above local-two. That
    is about the inverse of their backend calls per request
    (0.347 ÷ 0.279 = 1.24), as the plan predicts.
  - Leases are predicted to add nothing over shared: their extra fill saving
    is under 3% at these rates.
  - local-one is predicted two rungs above shared. One gateway's single L1
    sees all the reuse, and it has the pooled permits too.
- **Every predicted failure** is backend-bound:
  - `wait_timeout` 503s, at 67–80% utilization;
  - model p99 270–290 ms, far under the 750 ms bound.
- **Resolution:** one rung is 10%. A ceiling difference of one rung is not
  called an effect.

## Planned runs

1. `mixed` control: `python bench/capacity-v3.py --workload mixed --delay 250
   --start 200 --host-run bench/results/v3/20261105-035703-v34`
2. `zipf`: the same with `--workload zipf --complete`. `--complete` also runs
   the Go tests, race tests, vet and build into the artifact.

Modes: local-one, local-two, shared, leased-v2. Grid ×1.1 from 200/s;
3 confirmations; generator concurrency 256; L1 1000 entries per gateway;
max-age 10 s.

## Results

Runs (all with 32 permits and a 250 ms backend, so 128 fills/s):
- [`20261105-041050-v34`](../../bench/results/v3/20261105-041050-v34/summary.json):
  `mixed`, 54 trials, SUCCESS.
- [`20261105-044743-v34`](../../bench/results/v3/20261105-044743-v34/summary.json):
  `zipf`, 50 trials, `FAILED`, then resumed to SUCCESS (see
  [harness incidents](#harness-incidents)).

Ceilings are rates at which the climb trial and all three confirmations met
the SLO. "First failure" is the lowest rung above the ceiling where any trial
failed.

### mixed

| Mode | Ceiling | Predicted | First failure | Bound | Backend calls/request | Backend utilization at ceiling | Scheduled p99 at ceiling |
|---|---|---|---|---|---|---|---|
| local-one | **472** | 519 | 519 (a confirmation; 18 `wait_timeout`) | backend | 0.199 | 73% | 257 ms |
| local-two | **390** | 429 | 429 (2 of 5 trials) | backend | 0.200 | 61% | 253 ms |
| shared | **390** | 429 | 429 (2 of 5 trials) | backend | 0.200 | 61% | 255 ms |
| leased-v2 | **390** | 429 | 429 (1 of 4 trials) | backend | 0.200 | 61% | 256 ms |

- **Two-gateway modes:** the three modes are identical, as predicted. Their
  ladders saw the same sequence of passes and failures. At 472/s, each failed
  with 26–27 permit 503s, against 27 predicted.
- **local-one** is 1.21× the two-gateway modes, exactly the predicted ratio
  (519 ÷ 429).
- **Failures:** every one was `wait_timeout` 503s. No trial dropped an arrival
  or came near the 750 ms latency bound, and the shared hit ratio was 0.0–0.1%.

### zipf

| Mode | Ceiling | Predicted | First failure | Bound | Backend calls/request | Shared hit ratio | Backend utilization at ceiling |
|---|---|---|---|---|---|---|---|
| local-one | 293 | 390 | 322 (front 503s during a host stall) | **host** | 0.281 | — | 64% |
| local-two | **242** | 266 | 266 (2 of 4 trials; 9 `wait_timeout` each) | backend | 0.352 | — | 67% |
| shared | **293** | 322 | 322 (1 of 2 trials) | backend | 0.288 | 17.6% | 66% |
| leased-v2 | **293** | 322 | 322 (1 of 2 trials) | backend | 0.281 | 17.7% | 64% |

- **Capacity gain:** shared and leased-v2 carry **1.21× local-two's load**
  (242 → 293 req/s) at the same backend budget, exactly the predicted ratio
  (322 ÷ 266).
- **The plan's prediction:** at 242/s, local-two's backend calls per request
  are 1.16× shared's and 1.19× leased-v2's. The ceiling ratio is within one
  rung of that inverse. At each mode's own ceiling, the calls per request are
  0.352 against 0.288 (1.22×).
- **Leases add nothing over shared at this scale,** as predicted. Same ceiling;
  2.5% fewer calls per request.
- **local-one is host-bound,** so its ceiling is not reported as gateway
  capacity.
  - **Climb:** it passed through 390/s and failed at 429/s with permit 503s
    (13 and 37), where the model predicted its first failure.
  - **Confirmations:** they then failed three times in a row:
    - 390/s: 36 permit 503s, with p99 410 ms. The model predicts 5 for that
      seed, so a host stall is the likely cause, even though the label rule
      counts permit 503s as backend.
    - 354/s: p99 1,188 ms. The generator's own timers froze for 849 ms in the
      last 3 s window.
    - 322/s: 32 front 503s, when a stall outlasted the front's 250 ms readiness
      probe, plus 22 permit 503s.
  - **Ceiling:** it confirmed at 293/s.
  - **Its backend-bound ceiling is somewhere between 293 and 390/s.** These
    trials ran late in the session, after 2 hours of continuous load.

### Model check (after the runs)

The pre-run predictions used the climb trial's seed only. Every ceiling the
backend set came out exactly one rung below its prediction: the confirmation
seeds (45–47) are harder streams at the same rate.
[`check-capacity-v3.py`](../../bench/check-capacity-v3.py) replays every trial's
own seed through the same model (`model-check.json` in each run):

| Run | SLO verdicts agreeing with the model | Backend fills against the model |
|---|---|---|
| mixed | 52 of 54 | within 0.16% per trial (median 0.00%) |
| zipf | 44 of 50 | within 1.29% (median 0.20%) |

- **The 8 disagreements, all measured worse than modelled:**
  - **Five are borderline,** within 1–10 rejections of the 0.1% allowance:
    - mixed 429/s, seed 43: 14 and 15 against 5;
    - zipf local-two 266/s: 9 against 5, and 9 against 7;
    - zipf leased-v2 322/s: 10 against 9.

    The model ignores network, Redis and poll time.
  - **The other three** are local-one's host-affected zipf confirmations.
- **Where the model is exact:** away from host stalls, rejections are close to
  deterministic. Arrivals are scheduled, keys are seeded, and the fill time is
  fixed. Examples: mixed 472/s 27 against 27, 571/s 50 against 46, 519/s
  18 against 18.
- **Ceilings sit below 80% utilization:** each gateway has 16 permits and a
  50 ms wait, so random fill arrivals overflow the queue at 61–73% utilization.

### Harness incidents

- **Redis port collision (zipf, trial 9):** Redis could not bind its port
  inside WSL ("Address already in use"), and the run stopped with `FAILED`
  after 8 recorded trials.
  - **Cause:** the fixture allocator picks a port free on Windows. That port is
    also in Linux's ephemeral range, and WSL's localhost forwarding opens many
    connections inside the VM, so one of them can already be using it.
  - **Fix:** `run-v3.py` now retries Redis on a fresh port when (and only when)
    the Redis log shows that error, before any gateway starts. Each trial
    records `redis_port_retries`.
  - **Tests:** a stub test checked the decisions: a collision is retried, an
    unrelated exit still raises, and three consecutive collisions raise.
  - **Resume:** the run was resumed with identical settings and hashes. Its 8
    trials were replayed, not rerun, and its `FAILED` marker and logs are kept.
    None of the 42 later trials needed a retry.
- **Host stalls:** described under the host control and zipf local-one above.
  No backend-bound mode trial dropped an arrival.

### Checks

- **zipf run's `--complete`:** 10/10 ordinary and race-test packages; empty vet
  and build logs.
- **Loadgen tests,** including `TestLateFinalArrivalsAreSent`. It fails on the
  old generator and passes on the new one.
- **V3.3's five recorded `predictions.json`** recompute identically with the
  extended predictor.
- **Functional checks:**
  - `034409` (`--quick` zipf): both modes fail at the lowest rung, as
    predicted, with cold 1 s warmups;
  - `034519` (`--quick` mixed): the climb, the top of the grid and the
    confirmations, plus a replay-only `--resume` that reproduces the summary;
  - offline checks of every ladder path (retry, a failed confirmation, descent,
    top of grid, failing everywhere).

## Predictions against results

| Prediction | Result |
|---|---|
| Plan: `mixed` ceilings within noise of each other | **Held** for the three two-gateway modes (identical). **Not** for local-one, 21% higher, as the pooled-permit model predicted before the run |
| Plan: `zipf` shared/local-two ceiling ratio ≈ inverse backend calls/request ratio | **Held** within one rung: 1.21 against 1.16–1.22 |
| Model: mixed local-one/local-two 1.21, zipf shared/local-two 1.21 | **Held** exactly |
| Model: absolute ceilings | **One rung (10%) high** for all 7 backend-set ceilings. The confirmation seeds are harder than the climb seed |
| Model: leases add no capacity over shared on zipf | **Held** (293 and 293) |
| Model: every first failure is `wait_timeout`, at p99 far under 750 ms | **Held,** except zipf local-one's host-affected confirmations |

## Acceptance

- **Every ceiling is labelled:**
  - 7 are backend-bound and at most half the 1,250/s host ceiling;
  - 1 is host-bound (zipf local-one), and it is not reported as gateway
    capacity.
- **Aggregate budget equal across modes:** 32 permits, 32 waiter slots,
  64 flights and 1,024 cache waiters, split evenly; 1,000 L1 entries per
  gateway; 50 ms permit wait.
- **Predictions written before the runs:** in the sections above and in each
  run's `predictions.json`.

## What this shows

- **On a workload with cross-gateway reuse, the shared tier raises capacity.**
  At the same backend budget, shared and leased-v2 carry 21% more load than
  independent gateways on `zipf` before the SLO breaks (242 → 293 req/s). That
  is about their backend-call saving, as predicted.
- **On `mixed`, they add nothing,** and they cost no capacity either.
- **Splitting a fixed budget across gateways costs capacity.** One gateway with
  all 32 permits carries 21% more `mixed` load than two with 16 each, and it
  did so in the zipf climb too. Queueing on half-size pools overflows earlier.
  A real deployment splits permits per node, so this is the cost the shared
  tier works against.
- **This laptop cannot measure throughput above about 1,250 req/s at a zero-drop
  SLO.** Its 100–850 ms stalls, not CPU, set that limit.
