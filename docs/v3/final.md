# V3 final comparison

- **Where gateways repeat each other's keys, sharing a cache through Redis pays off:** 18–35% fewer backend calls, 19–42% with leases, and on Zipf 21% more load at the same SLO and backend budget.
- **The lease protocol's latency penalty is gone:** Redis round trips per fill fell from 5 to 2, taking 1.3–2.1 ms off leased p95 and bringing it within noise of plain shared caching.
- **Without reuse, neither tier helps:** they only add 1–2 ms at p95. Every number comes from one laptop, whose stalls cap it at 1,250 req/s.

V3 turned V2's correct but benefit-free mechanisms into measured results. It removed
avoidable coordination cost, tested workloads with cross-gateway reuse, and measured
capacity instead of a fixed rate every mode already met. V3.5 (hash routing) was
optional and was not run.

## What was compared

| Mode | Gateways | Redis | What it represents |
|---|---:|---|---|
| local-one | 1 | none | One gateway holding the whole budget |
| local-two | 2 | none | Independent gateways, budget split in half: the baseline for every comparison |
| shared | 2 | responses | A gateway can read a fill another gateway has finished |
| leased (v1) | 2 | responses and leases | A fill in flight on any gateway makes the others wait. V2's protocol: 5 Redis operations per fill |
| leased-v2 | 2 | responses and leases | The same leases with V3.2's protocol: 2 operations per fill |
| local-two-aa | 2 | none | A second copy of local-two in every block; the spread between the copies is the noise floor |

| Workload | Keys | Why it is here |
|---|---|---|
| mixed | 80% one hot key, 20% never repeated | Control: no cross-gateway reuse for a shared tier to use |
| zipf | 10,000 keys, exponent 1.1 | Skewed reuse across gateways |
| hotset | 100 equally likely keys, 1 s TTL | Frequent expiry, so both gateways often miss the same key together |
| hot | One key | Host-ceiling control only |

Every mode gets the same aggregate budget: 32 backend permits, 32 permit-queue slots,
64 cache flights and 1,024 cache waiters, split evenly across gateways. Each gateway
has a 1,000-entry L1. A *fill* is one backend call that populates the cache.

## Backend work at 500 req/s

Medians of 5 blocks of 15,000 requests, 50 ms backend. "Predicted" comes from
[`predict-v3.py`](../../bench/predict-v3.py), which replayed each run's seeded key
stream before its first trial. "vs local-two" is the median same-block difference.

| Workload | Mode | Protocol | Backend calls | Predicted | vs local-two | Calls per request | Shared hit ratio | Redis ops per fill | Redis commands per fill |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| mixed | local-one | — | 2,999 | 2,999 | −3 (−0.1%) | 0.200 | — | 0.00 | — |
| mixed | local-two | — | 3,002 | 3,002 | — | 0.200 | — | 0.00 | — |
| mixed | shared | — | 3,002 | 3,002 | 0 | 0.200 | 0.0% | 2.00 | 3.00 |
| mixed | leased | v1 | 2,999 | 2,999 | −3 (−0.1%) | 0.200 | 0.0% | 5.01 | 12.00 |
| mixed | leased-v2 | v2 | 2,999 | 2,999 | −3 (−0.1%) | 0.200 | 0.1% | 2.00 | 8.01 |
| zipf | local-one | — | 3,755 | 3,756 | −627 (−14.3%) | 0.250 | — | 0.00 | — |
| zipf | local-two | — | 4,382 | 4,381 | — | 0.292 | — | 0.00 | — |
| zipf | shared | — | 3,594 | 3,593 | −789 (−18.0%) | 0.240 | 20.6% | 2.26 | 4.03 |
| zipf | leased-v2 | v2 | 3,564 | 3,561 | −818 (−18.7%) | 0.238 | 20.9% | 2.28 | 9.11 |
| hotset | local-one | — | 2,497 | 2,498 | −1,795 (−41.8%) | 0.166 | — | 0.00 | — |
| hotset | local-two | — | 4,295 | 4,300 | — | 0.286 | — | 0.00 | — |
| hotset | shared | — | 2,787 | 2,766 | −1,508 (−35.1%) | 0.186 | 42.0% | 2.72 | 5.90 |
| hotset | leased-v2 | v2 | 2,494 | 2,498 | −1,797 (−41.8%) | 0.166 | 44.4% | 3.07 | 12.14 |

- **mixed** is from the final run; **zipf** and **hotset** are from V3.3.
- **Protocol v1** ran on `mixed` only. V3.3 used v2 for leases, after V3.2 accepted it.
- **Ops per fill rise above 2 on reuse workloads** because a lookup that hits costs
  an operation without causing a fill.
- **Commands exceed operations** because Redis also counts the commands a Lua script
  runs.

## Latency at 500 req/s

Median same-block difference from local-two, in ms, shown as p95 / p99. `*` marks a
difference beyond that run's noise floor.

| Workload | Floor p95 / p99 | local-one | shared | leased (v1) | leased-v2 | leased-v2 − shared |
|---|---|---|---|---|---|---|
| mixed | 0.76 / 3.13 | +0.12 / +0.06 | +1.29\* / +1.37 | +2.64\* / +3.30\* | +1.34\* / +1.54 | +0.05 / +0.15 |
| zipf | 0.38 / 0.59 | −0.12 / −0.08 | +1.55\* / +1.90\* | not run | +1.69\* / +2.07\* | +0.26 / +0.59\* |
| hotset | 0.21 / 0.40 | −0.24\* / −0.14 | +0.94\* / +1.18\* | not run | +0.98\* / +1.52\* | +0.06 / +0.34 |

- **The Redis tier costs 1–2 ms at p95,** one round trip through WSL2 on every L1 miss.
- **Leases against shared:** within the floor at p95 on all three workloads. At p99,
  zipf's +0.59 ms just reaches that run's 0.59 ms floor.
- **mixed p99:** one stalled A/A block set the final run's p99 floor at 3.13 ms, so
  only protocol v1's +3.30 ms clears it. The other four A/A blocks differed by at most
  0.18 ms.

## Lease protocol v1 against v2

`mixed`, both protocols in the same blocks. Latency is paired, shown as p95 / p99 in ms.

| Run | Blocks | Ops per fill | Commands per fill | v2 − v1 | Blocks where v2 p95 was slower | v2 − shared | Floor |
|---|---:|---|---|---|---|---|---|
| V3.2 acceptance | 6 | 5.00 → 2.00 | 12.01 → 8.01 | −2.11\* / −3.77\* | 0 of 6 | +0.07 / −0.05 | 0.72 / 2.03 |
| Final run | 5 | 5.01 → 2.00 | 12.00 → 8.01 | −1.31\* / −1.77 | 0 of 5 | +0.05 / +0.15 | 0.76 / 3.13 |

- **Operations and commands reproduced.** Redis also counts the commands a script
  runs, so commands fell 33%, not the 60% the plan first expected.
- **The latency gain reproduced at a smaller size.** v2 beat v1 at p95 in every block
  of both runs, by a median of 2.11 ms and then 1.31 ms.
- **The p99 gain did not clear the final run's floor.** v2 was faster at p99 in all
  5 blocks, but the −1.77 ms median is inside the 3.13 ms floor.
- **leased-v2 stayed within noise of shared** in both runs.

## Capacity at a fixed SLO

The highest rate each mode sustained with at least 99.9% success, scheduled p99 at
most 3× the backend delay, and no generator drops. Each ceiling needed a passing climb
trial and 3 passing confirmations, on a ×1.1 rate grid, so one step is 10% and a
one-step difference is not called an effect.

The host-ceiling control ran first: 1,250 req/s. Backend capacity was then set so
every predicted ceiling stayed at or below half of that, giving 32 permits / 250 ms,
or 128 fills/s.

| Workload | Mode | Ceiling (req/s) | Predicted | First failure | Bound by | Calls per request | Backend utilization | Shared hit ratio | Scheduled p99 |
|---|---|---:|---:|---:|---|---:|---:|---:|---:|
| mixed | local-one | **472** | 519 | 519 | backend | 0.199 | 73% | — | 257 ms |
| mixed | local-two | **390** | 429 | 429 | backend | 0.200 | 61% | — | 253 ms |
| mixed | shared | **390** | 429 | 429 | backend | 0.200 | 61% | 0.0% | 255 ms |
| mixed | leased-v2 | **390** | 429 | 429 | backend | 0.200 | 61% | 0.1% | 256 ms |
| zipf | local-one | 293 | 390 | 322 | host | 0.281 | 64% | — | 253 ms |
| zipf | local-two | **242** | 266 | 266 | backend | 0.352 | 67% | — | 260 ms |
| zipf | shared | **293** | 322 | 322 | backend | 0.288 | 66% | 17.6% | 258 ms |
| zipf | leased-v2 | **293** | 322 | 322 | backend | 0.281 | 64% | 17.7% | 257 ms |

- **zipf:** shared and leased-v2 carry 1.21× local-two's load, the predicted ratio.
  That is about the inverse of their backend calls: at 242 req/s, local-two makes
  1.16× shared's and 1.19× leased-v2's calls per request.
- **mixed:** the three two-gateway modes are identical.
- **local-one on mixed** carries 21% more because one queue of 32 permits overflows
  later than two queues of 16.
- **zipf local-one is not gateway capacity.** It passed its climb to 390 req/s, then
  failed confirmations at 390, 354 and 322. The last two were host-caused, and the
  first very likely was.

## Restart

`zipf` at 500 req/s, 3 trials per mode. Gateway 1 stops at 15 s and returns with an
empty L1 at 20 s. Backend calls, median of 3:

| Mode | 10 s before stop | Steady per 2 s | First 2 s after return | Predicted | 10 s after return | Front 502s (3 trials) |
|---|---:|---:|---:|---:|---:|---:|
| local-two | 1,477 | 295 | **345** | 388 | 1,519 | 62 |
| shared | 1,228 | 246 | **249** | 249 | 1,230 | 60 |
| leased-v2 | 1,212 | 242 | **241** | 248 | 1,221 | 32 |

Calls are totals across both gateways. Without Redis, they run 17% above the steady
rate in the empty gateway's first 2 s; about 60 more fills landed just before that
window began, so this understates the burst. With Redis, they stay at the steady rate.
Over 10 s the effect is small for every mode: with a 10 s TTL, an empty L1 mostly moves
fills earlier.

## Noise floor

The largest same-block difference between the two identical local-two copies, in ms.
No smaller difference in that run is called an effect.

| Run | Artifact | Blocks | p95 | p99 |
|---|---|---:|---:|---:|
| V3.1 baseline | [`20261104-051315-v3`](../../bench/results/v3/20261104-051315-v3/summary.json) | 5 | 0.40 | 2.63 |
| V3.2 acceptance | [`20261104-153628-v3`](../../bench/results/v3/20261104-153628-v3/summary.json) | 6 | 0.72 | 2.03 |
| V3.3 zipf | [`20261104-175819-v3`](../../bench/results/v3/20261104-175819-v3/summary.json) | 5 | 0.38 | 0.59 |
| V3.3 hotset | [`20261104-192932-v3`](../../bench/results/v3/20261104-192932-v3/summary.json) | 5 | 0.21 | 0.40 |
| V3.3 mixed | [`20261104-194601-v3`](../../bench/results/v3/20261104-194601-v3/summary.json) | 5 | 0.47 | 0.42 |
| Final run | [`20261105-142924-v3`](../../bench/results/v3/20261105-142924-v3/summary.json) | 5 | 0.76 | 3.13 |

CPU pinning made the floor worse on this hybrid laptop: 6.73 / 87.69 ms pinned against
1.02 / 11.81 ms unpinned in an A/A-only test, so it is opt-in.

## Predictions against measurements

Every prediction was written before its run.

| Milestone | Prediction | Measured | Verdict |
|---|---|---|---|
| V3.2 | Operations per fill 5 → 2 | 5.00 → 2.00 | Held |
| V3.2 | Plan: Redis commands −60% | −33% (12.01 → 8.01) | **Wrong.** It assumed one command per operation; corrected before the run |
| V3.2 | Leased p95 within the floor of shared | +0.07 ms (floor 0.72), then +0.05 ms in the final run (floor 0.76) | Held twice |
| V3.3 | zipf backend calls −18.0% / −18.7% | −18.0% / −18.7% | Held |
| V3.3 | hotset backend calls −35.7% / −41.9% | −35.1% / −41.8% | Held; shared overlap was 9% above the model |
| V3.3 | Every workload's fills | Within 1% per block | Held |
| V3.3 | Restart: about +92 calls in the first 2 s without Redis, about +4 with | +28 to +63, and −37 to +18 | Direction held; size not cleanly measured, because about 60 fills landed before the ready snapshot |
| V3.3 | hotset leases raise p99 | +0.34 ms, within the floor | **Did not appear** |
| V3.3 | Backend-bound zipf capacity up about 23% | +21% | Held |
| V3.4 | Plan: `mixed` ceilings within noise | Two-gateway modes identical; local-one +21% | **Not for local-one,** which the model predicted before the run |
| V3.4 | Model: ceiling ratios 1.21 (mixed local-one, zipf shared) | 1.21 and 1.21 | Held |
| V3.4 | Model: absolute ceilings | All 7 backend-set ceilings one rung (10%) low | **Missed.** Predictions used the climb seed; confirmation seeds are harder. Each trial's own seed agrees with 96 of 104 verdicts |
| V3.4 | Leases add no capacity over shared | 293 and 293 | Held |

## Unfavorable results

**Benefits that did not appear, or shrank:**
- `mixed` gains nothing from either tier, and pays the latency cost above.
- Protocol v2's first measured gain did not fully reproduce: −2.11 ms at p95 in V3.2,
  −1.31 ms in the final run, and its p99 gain did not clear the final run's floor.
- Leases add no capacity over shared on zipf, and save only 26 fills per block.
- Against one gateway with the whole budget, the tier mostly recovers what splitting
  lost. At 500 req/s, zipf shared and leased-v2 make 4.3% and 5.1% fewer backend calls
  than local-one. On hotset, leased-v2 only ties it, and shared makes 11.6% more.
- One gateway beats two on `mixed` capacity by 21%. Splitting a fixed permit budget
  costs capacity.
- The restart benefit lasts about 2 s, and front 502s happen with or without Redis
  (7–30 per restart trial).

**Availability:**
- Leases turn a Redis timeout into a client 503 under the protection-first policy:
  2 in the final run and 4 in V3.3's mixed run, each from a Redis operation exceeding
  its 50 ms deadline. Shared mode absorbs such errors. In the final run, 4 dependency
  errors there caused no client failures, because a failed lookup counts as a miss
  and goes to the backend.

**Limits on the measurements:**
- The host ceiling is only 1,250 req/s, set by stalls of 100–850 ms rather than CPU
  (1.3 cores). So capacity ran behind a 250 ms backend instead of the plan's 100 ms,
  and at 61–73% backend utilization. The model predicted a larger gain at 100 ms
  (1.46×), which this host cannot test.
- That ceiling is noisy: 2,441 req/s passed 3 of 4 trials but never confirmed.
- zipf local-one has no backend-bound ceiling.
- A host stall can look like a backend failure. zipf local-one at 390 req/s had 36
  permit 503s where the model expected 5.
- One mixed block in V3.3 stalled: 536 late arrivals and 4 leased-v2 timeout 503s.
- One laptop, Redis inside WSL2 (about 1 ms per operation), no real network, and
  VS Code using 1–1.6 cores during the capacity runs. The A/A spread is run-specific,
  not a confidence interval.

**Not done:**
- hash routing (V3.5, optional) and the optional 4-gateway run;
- protocol v1 on zipf and hotset, and a capacity ladder for v1;
- a capacity ladder on hotset, so leases' capacity effect is untested where they save
  the most fills.

**Runs that failed or were rejected, all kept:**
- The first V3.1 baseline (pinned) was rejected: floor 61.75 / 373.24 ms, 150 front 503s.
- A V3.3 functional check failed on a harness log-reading bug.
- The V3.3 zipf run was interrupted by Modern Standby, then resumed.
- The first V3.4 host control was confounded: the hot key's 100 ms expiry fill filled
  the generator's 256 in-flight slots at 2,500 req/s.
- A second host control found no ceiling because it started too high.
- The V3.4 zipf run stopped on a Redis port collision inside WSL, then resumed.

## Method

- **Topology:** a scheduled open-loop generator sends through a round-robin front proxy
  to the gateways, which share one mock backend and, in Redis modes, one Redis 7.4.2.
  Every process is separate, on one Windows laptop, unpinned, on AC power.
- **Blocks:** every mode runs once per block with the same seed, in rotating order.
  Steady trials have 5 s of warmup and 30 s measured.
- **Reconciliation:** each trial's totals must agree across the generator, front,
  gateways and backend, or the run fails.
- **Code identity:** manifest hashes show gateway, front and backend source are
  identical across the V3.3, V3.4 and final runs. Only the generator and the harness
  changed between them. V3.4 changed the generator to send arrivals due just before
  the window ends instead of dropping them; no steady V3.3 trial had dropped one.

## Reproduce

Requires Go 1.25 or later, Python 3, a C compiler for race builds, and Redis 7.4.2
built at `bin/redis-build/redis-server` (on Windows, inside WSL2). See
[the pinned build](../v2/faults.md#reproduce-the-fault-matrix).

```powershell
# One command: the six-mode mixed comparison, cold bursts, a 5-minute soak, tests,
# race tests, all ten fault cases under v1, v2 and v2-race, and a source-only rebuild.
python bench/run-v3.py --complete

# Reuse workloads.
python bench/run-v3.py --workload zipf --modes local-one,local-two,shared,leased-v2,local-two-aa --cold-trials 0 --soak 0 --restart-trials 3 --complete
python bench/run-v3.py --workload hotset --max-age 1 --modes local-one,local-two,shared,leased-v2,local-two-aa --cold-trials 0 --soak 0

# Capacity: the host control first, then the ladders labelled against it.
python bench/capacity-v3.py --workload hot --modes local-two --delay 1 --slo-p99-ms 300 --start 1000 --step 1.25
python bench/capacity-v3.py --workload mixed --delay 250 --start 200 --host-run bench/results/v3/<host run>
python bench/capacity-v3.py --workload zipf --delay 250 --start 200 --host-run bench/results/v3/<host run> --complete
python bench/check-capacity-v3.py bench/results/v3/<capacity run>
```

Results will differ on other hardware. Backend calls should reproduce closely; latency
and the host ceiling will not.

## Verification and artifact index

**Final run** [`20261105-142924-v3`](../../bench/results/v3/20261105-142924-v3/summary.json), the
one-command reproduction at commit `7957e01`, from a clean tree, ended `SUCCESS`:
- **49 trials reconciled:** 30 steady, 18 cold bursts and 1 soak. Every mode's median
  backend calls equalled its prediction.
- **Cold bursts, 100 clients, backend calls per trial:** local-one [1, 1, 1], local-two [2, 2, 2], shared [2, 2, 2], leased [1, 1, 1], leased-v2 [1, 1, 1], local-two-aa [2, 2, 2].
- **Soak:** leased-v2, 5 minutes: 150,000 of 150,000 requests
  succeeded, with 0 rejected, 0 generator drops, p99 53.8 ms,
  2.00 Redis operations and 8.01 commands per fill.
- **Tests:** 10/10 ordinary and 10/10 race-test packages; empty vet, build and
  race-build logs.
- **Faults:** all ten V2.6 fault cases passed under protocol v1, v2, and v2 with a
  race-built gateway (30 runs).
- **Clean rebuild:** a source-only snapshot rebuilt and passed the same comparison at
  smoke-test length.

| Evidence | Run |
|---|---|
| Final comparison, faults, clean rebuild | [`20261105-142924-v3`](../../bench/results/v3/20261105-142924-v3/summary.json) |
| V3.1 baseline | [`20261104-051315-v3`](../../bench/results/v3/20261104-051315-v3/summary.json) |
| V3.2 protocol acceptance | [`20261104-153628-v3`](../../bench/results/v3/20261104-153628-v3/summary.json) |
| V3.3 zipf and restart | [`20261104-175819-v3`](../../bench/results/v3/20261104-175819-v3/summary.json) |
| V3.3 hotset | [`20261104-192932-v3`](../../bench/results/v3/20261104-192932-v3/summary.json) |
| V3.3 mixed | [`20261104-194601-v3`](../../bench/results/v3/20261104-194601-v3/summary.json) |
| V3.4 host ceiling | [`20261105-035703-v34`](../../bench/results/v3/20261105-035703-v34/summary.json) |
| V3.4 mixed capacity | [`20261105-041050-v34`](../../bench/results/v3/20261105-041050-v34/summary.json) |
| V3.4 zipf capacity | [`20261105-044743-v34`](../../bench/results/v3/20261105-044743-v34/summary.json) |

Every V3 run, including failed ones, is listed in the
[results index](../../bench/results/v3/README.md). Milestone reports:
[V3.0 harness](README.md), [V3.1 noise floor](noise-floor.md),
[V3.2 lease protocol](lease-protocol.md), [V3.3 reuse workloads](reuse-workloads.md),
[V3.4 capacity](capacity.md). V2.7 numbers stay historical in
[the V2 report](../v2/final.md) and are not recalculated here.
