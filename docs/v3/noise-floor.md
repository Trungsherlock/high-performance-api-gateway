# V3.1 — Noise floor and the V3 baseline

V3.1 measures how much two identical runs differ on this laptop, then reruns the
V2.7 four-mode comparison under that yardstick. Later V3 milestones compare
against this baseline, not against V2.7. Like V3.0, it does not change caching,
coalescing, admission or lease behavior.

## Harness changes

**A/A control.** `local-two-aa` is a second copy of `local-two`, with the same
binaries, topology, budget and seed. It runs in every block by default.
`--modes` refuses `local-two-aa` without `local-two`.

**Five blocks, rotated.** `--trials` now defaults to 5. The order of modes still
rotates each block, and the seed is shared within a block. With five modes and
five blocks, every mode runs once in every position.

**Paired differences.** For each block, [summarize-v3.py](../../bench/summarize-v3.py)
subtracts `local-two` from every other mode. That holds seed, block and time
roughly constant. It reports the median, minimum, maximum and per-block values
for send p50/p95/p99, scheduled p99 and backend calls, plus how many blocks were
positive.

**Noise floor.** For each metric, the floor is the largest |`local-two-aa` −
`local-two`| over the blocks at that rate. A difference counts as an effect
(marked `*`) only if its median paired value exceeds the floor. With five
blocks, the largest of five A/A differences is a crude, conservative bound. It
is not a confidence interval. Runs without the A/A copy print no floor and a
warning instead.

**CPU pinning (opt-in after measurement).** `--affinity auto` puts whole
physical cores, from the fastest class, into three disjoint sets. It was the
default for the first V3.1 runs. Those runs showed it made noise worse (see
[Results](#results)), so the default is now `--affinity off`. The [host
module](../../bench/host.py) reads the core layout from
`GetLogicalProcessorInformationEx` on Windows and from sysfs on Linux.

| Set | Processes | Core Ultra 7 155H (this laptop) | `GOMAXPROCS` |
|---|---|---|---:|
| `gateways` | every gateway | P-cores, logical CPUs 10–15 | 6 |
| `generator` | `loadgen`, `burst` | P-cores, logical CPUs 16–19 | 4 |
| `support` | backend, front proxy | P-core, logical CPUs 0–1 | 2 |
| unpinned | Redis (WSL2 VM), Python harness, OS | E-cores 2–9, LP E-cores 20–21 | — |

- Windows reports EfficiencyClass 1 for the six P-cores and 0 for the E-cores.
  The core holding CPU 0 sorts last because it usually services the most
  interrupts, so it lands in `support`.
- On Windows, `SetProcessAffinityMask` runs right after start and also resets
  every existing thread's mask. Go sizes `GOMAXPROCS` before that, so each
  process also gets `GOMAXPROCS` equal to its set size, which turns off Go
  1.25's automatic resizing. On Linux, the set is applied before `exec`.
- Every launch reads the mask back and asserts it matches. The trial's
  `commands` record holds each process's set and `GOMAXPROCS`.
- Gateways share one set, so the gateway tier gets the same CPUs whether there
  is one gateway or several, just as it gets the same aggregate budget.
- Redis runs inside the WSL2 VM, whose virtual CPUs the Windows scheduler can
  place on any core, pinned or not. Pinning cannot prevent this.
- Other values: `--affinity off` leaves everything unpinned. Explicit sets look
  like `generator=0-3;gateways=4-9;support=10-11`. A resume requires the same
  resolved sets.

**Power.** The manifest records the active power plan, the Windows 11 power-mode
overlay and AC line state. Each trial also records them at its start. The
summary counts trials not confirmed on AC power.

## Predictions

These were written before any measured V3.1 run. The basis is V2.7's three
trials per mode: p95 medians were 54.8 / 54.2 / 56.5 / 60.4 ms for local-one /
local-two / shared / leased. The V3.0 single block gave paired p95 differences
of +3.1 ms (shared) and +5.1 ms (leased).

1. **Noise floor.** The largest A/A difference will be 1–3 ms for send p95 and
   5–25 ms for send p99.
2. **Paired p95 against local-two:**
   - leased: +4 to +8 ms, beyond the floor (the extra lease round trips);
   - shared: +1 to +4 ms, close to the floor, so it could fall either side;
   - local-one: 0 to +2 ms, within noise.
3. **Paired p99.** Only leased may exceed the floor. The others will be within noise.
4. **Counts are unaffected by pinning:**
   - backend calls within V2.7's ranges;
   - A/A backend-call differences under 1%;
   - leased about 3 fills below local-two per block;
   - 2.00 and 5.00 dependency operations per fill.
5. **Pinning.** Two A/A-only runs, one unpinned and one pinned, test this.
   - The pinned run will have fewer late arrivals and a smaller p99 floor.
   - The p95 floor will change by less than 1 ms, because the 50 ms backend
     delay dominates p95.
   - Absolute p50 will move by less than 1 ms. At 500 requests/s, six gateway
     CPUs are not a constraint.

## Runs

All runs used the same working tree on top of `ac24ab8`, with the same binaries,
AC power, the Balanced plan and the balanced power-mode overlay. Each passed
reconciliation (`SUCCESS`).

| Run | Modes | Affinity | Purpose |
|---|---|---|---|
| [`20261104-043055-v3`](../../bench/results/v3/20261104-043055-v3/summary.json) | 5 | auto | `--quick` functional check (2-second trials; latencies not used) |
| [`20261104-043318-v3`](../../bench/results/v3/20261104-043318-v3/summary.json) | 5 | auto | First baseline, `--complete`. **Rejected as the baseline:** pinning noise |
| [`20261104-045906-v3`](../../bench/results/v3/20261104-045906-v3/summary.json) | A/A only | off | Pinning test, unpinned arm |
| [`20261104-050522-v3`](../../bench/results/v3/20261104-050522-v3/summary.json) | A/A only | auto | Pinning test, pinned arm |
| [`20261104-051315-v3`](../../bench/results/v3/20261104-051315-v3/summary.json) | 5 | off | **V3 baseline**, `--complete` |
| [`20261104-053815-v3`](../../bench/results/v3/20261104-053815-v3/summary.json) | A/A only | gateways 10–15, generator 16–19, support 2–3 | Diagnostic: the auto layout with backend and front moved off CPU 0's core |

A/A-only runs used `--modes local-two,local-two-aa --cold-trials 0 --soak 0`
and 5 blocks each.

## Results

### Pinning made noise worse

| Run | Affinity | p95 floor | p99 floor | Late arrivals per trial | Median send p50 | Front 503s |
|---|---|---:|---:|---|---:|---:|
| 5-mode, `043318` | auto | 61.75 ms | 373.24 ms | 409–1,875 | 1.45 ms | 150 (3 trials) |
| 5-mode, `051315` | off | **0.40 ms** | **2.63 ms** | 12–495 | 0.63 ms | 0 |
| A/A only, `045906` | off | 1.02 ms | 11.81 ms | 46–351 | 0.66 ms | 0 |
| A/A only, `050522` | auto | 6.73 ms | 87.69 ms | 340–1,056 | 1.19 ms | 0 |
| A/A only, `053815` | support off CPU 0's core | 1.14 ms | 53.01 ms | 499–724 | 1.26 ms | 0 |

- **Pinned runs were worse on every measure:** larger floors, more late
  arrivals, and about twice the p50.
- **Time of day doesn't explain it.** The unpinned A/A run sat between two
  pinned runs.
- **The front-proxy 503s point to long stalls.** In the first pinned baseline,
  the front proxy marked gateways unavailable in 3 trials (45, 23 and 82
  requests). That happens when its 250 ms readiness probes time out, so
  processes stalled for more than a quarter-second.
- **CPU 0's core explains only part of it.** Moving backend and front off that
  core:
  - removed the front 503s;
  - brought the p95 floor down to about the unpinned A/A run's (1.14 against 1.02 ms);
  - left late arrivals, p50 and the p99 floor well above unpinned.
- **Cause not isolated.** Restricting each process to a few cores on this hybrid
  Windows laptop adds scheduling delay that the OS scheduler otherwise avoids.
  That is the likeliest reading, but not a proven one.
- **The floor differs between unpinned runs.** The 5-mode baseline's floor
  (0.40 / 2.63 ms) is below the A/A-only run's (1.02 / 11.81 ms). Each run must
  therefore carry its own A/A copy, which is now the default. Later milestones
  judge effects against the floor of the same run.

**Decision:** `--affinity` defaults to `off`. `auto` and explicit sets remain
for other hosts. The first pinned baseline is kept as an unfavorable result.

### The V3 baseline

Run `20261104-051315-v3` used the defaults: 5 blocks, `mixed` workload,
500/s, 2 gateways, unpinned. It also ran cold bursts, the soak, tests, race
tests, the ten fault cases and the clean-source rebuild, and all passed.

Medians over 5 blocks, with ranges in brackets:

| Mode | Send p95 ms | Send p99 ms | Backend calls | Dependency ops | Ops per fill |
|---|---|---|---|---|---:|
| local-one | 51.59 [51.41, 51.97] | 52.85 [52.21, 56.71] | 2,999 [2,969, 3,078] | 0 | 0.00 |
| local-two | 51.72 [51.62, 51.77] | 52.32 [52.07, 54.12] | 3,002 [2,972, 3,081] | 0 | 0.00 |
| shared | 53.60 [52.89, 53.77] | 55.44 [54.14, 56.90] | 3,002 [2,972, 3,081] | 6,004 | 2.00 |
| leased | 56.03 [55.79, 59.71] | 58.14 [57.94, 161.19] | 2,997 [2,969, 3,078] | 15,003 | 5.01 |
| local-two-aa | 51.83 [51.75, 52.02] | 54.20 [52.66, 54.70] | 3,002 [2,972, 3,081] | 0 | 0.00 |

**Noise floor** (largest |A/A| difference over 5 blocks):
- send p50 0.14 ms, p95 0.40 ms, p99 2.63 ms;
- scheduled p99 4.02 ms;
- backend calls 0.

Paired differences from `local-two` in the same block. Each cell is the median
[min, max]; positive blocks are out of 5; `*` marks a median beyond the floor.

| Mode | Send p95 ms | Send p99 ms | Scheduled p99 ms | Backend calls |
|---|---|---|---|---|
| local-one | −0.06 [−0.24, +0.20], 2/5 | +0.75 [−1.91, +3.76], 4/5 | +0.84 [−2.23, +4.09] | **−3\*** [−3, −3] |
| shared | **+1.93\*** [+1.12, +2.04], 5/5 | **+3.37\*** [+0.02, +3.95], 5/5 | +3.34 [−0.23, +4.05] | 0 [0, 0] |
| leased | **+4.39\*** [+4.02, +8.08], 5/5 | **+5.86\*** [+4.02, +109.12], 5/5 | **+5.88\*** [+3.80, +167.64] | **−3\*** [−5, −3] |
| local-two-aa | +0.09 [+0.06, +0.40], 5/5 | +0.34 [−0.17, +2.63], 4/5 | +0.38 [−0.17, +4.02] | 0 [0, 0] |

**Latency:**
- Shared costs about 1.9 ms at p95 against independent local gateways, and
  leased about 4.4 ms. Both were positive in every block.
- The leased−shared gap is about 2.5 ms. The 3 extra lease round trips are the
  candidate explanation, at about 0.8 ms each. V3.2 removes them.
- At p95, local-one cannot be told apart from local-two.

**Backend calls:**
- Leases and a single gateway save exactly the 3 synchronized hot-key expiries
  per trial, as in V2.7. Shared storage saves none.
- `mixed` has no cross-node reuse. That is the control V3.3 needs.

**Leased block 4 spike:**
- It had p99 161.19 ms and 495 late arrivals.
- Its 2 coordination 503s came from 2 Redis operations classified `timeout`.
- That one block sets the leased p99 maximum. Every other trial in this run
  had no rejections and no dependency errors.

**A/A sign:** the A/A copy was slightly slower in all 5 blocks at p50 and p95,
by 0.03–0.40 ms, even though its position rotates. The floor may include a
small systematic offset of unknown cause, not only random spread. It is far
below every effect marked above.

**Other checks:**
- Cold bursts made 1/2/2/1/2 backend fills, 3 of 3 per mode.
- L1 request hit ratios were 79.0–79.8%.
- Leased had 3 distributed followers per block.

**Soak** (leased, 5 minutes, unpinned):

| Measure | Result |
|---|---|
| Requests | 150,000 sent; **150,000 successes** |
| Rejections and errors | 0 rejections, 0 dependency errors, 0 generator drops |
| Send p50 / p95 / p99 | 0.66 / 56.30 / 59.66 ms |
| Backend calls | 30,031 |
| Ops per fill | 5.005 |

V2.7's soak had 3 coordination 503s. The first, pinned V3.1 soak had 27 (8
Redis `timeout` errors), which is the first attributed cause for that kind of
503. The unpinned soak had none.

## Predictions against results

| # | Prediction | Result | Verdict |
|---|---|---|---|
| 1 | Floor 1–3 ms at p95, 5–25 ms at p99 | Unpinned 5-mode 0.40 / 2.63 ms; unpinned A/A-only 1.02 / 11.81 ms | The A/A-only run held; the 5-mode floor came in lower than predicted |
| 2 | Paired p95: leased +4 to +8 beyond the floor; shared +1 to +4, either side; local-one 0 to +2 within noise | +4.39\*; +1.93\*; −0.06 | Held. Shared landed beyond the floor; local-one sat just below the range but within noise |
| 3 | At p99 only leased may exceed the floor | Leased +5.86\* and shared +3.37\* | Partly failed: shared also exceeded the floor, by 0.7 ms |
| 4 | Counts unaffected | Blocks 1–3, which share V2.7's seeds, match V2.7's backend calls exactly; leased block 3 is 2,969 against 2,968. Blocks 4–5 use new seeds (2,997–3,081). A/A difference 0; leased −3; 2.00 / 5.01 ops per fill | Held, pinned and unpinned |
| 5 | Pinning gives fewer late arrivals, a smaller p99 floor, a p95 floor within 1 ms and p50 within 1 ms | More late arrivals, larger floors (p95 +5.7 ms), p50 about doubled (+0.5 to +0.8 ms) | **Failed**, except that p50 stayed within 1 ms |

## Acceptance

- The noise floor for send p95/p99 is documented: 0.40 / 2.63 ms in the
  baseline run and 1.02 / 11.81 ms in a separate unpinned A/A run. Every V3 run
  now measures its own floor.
- The four-mode comparison, rerun under the V3 harness, is the V3 baseline:
  [`20261104-051315-v3`](../../bench/results/v3/20261104-051315-v3/summary.json).
  Its `--complete` checks passed.
- CPU pinning, the plan's proposed noise reduction, did not lower the floor on
  this host. It is opt-in and reported as unfavorable.
