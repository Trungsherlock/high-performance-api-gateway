# V3.3 — Workloads with cross-node reuse

V2.7 to V3.2 measured the shared tier and leases on `mixed`. In `mixed`, one hot
key stays in every gateway's L1, and the other keys are never requested twice.
So there was nothing for a shared tier to reuse. V3.3 adds workloads that repeat
keys across gateways, predicts their backend fills from the generator's own key
stream, and tests a gateway restart.

## What changed

**Generator** ([cmd/loadgen](../../cmd/loadgen/main.go)):
- **New workloads:**
  - `zipf`: keys `z0`…`z{N−1}` drawn with `rand.NewZipf` (`-keys N`, `-zipf-s S`;
    s must exceed 1).
  - `hotset`: keys `h0`…`h{K−1}`, equally likely (`-hot-keys K`).
- **Seeding:** both are seeded like the existing workloads. They draw one key per
  *offered* arrival, so dropped arrivals never shift later keys.
- **Old workloads unchanged:** `hot`, `mixed` and `unique` still draw per issued
  request, and a test checks their streams are identical.
- **`-plan-only`:** prints each phase's key stream as JSON and sends nothing.

**Gateway:** adds `-cache-entries` and `-cache-bytes`. The defaults, 1000 and
16 MiB, keep V2 behavior.

**Predictor** ([bench/predict-v3.py](../../bench/predict-v3.py)) replays the
planned stream (warmup, then measurement) through models of the gateways:
- **Common to every model:**
  - round-robin routing;
  - a 1000-entry LRU L1 per gateway;
  - TTL = min(max-age, 10 s);
  - each backend fill lasts the backend delay, and requests on the same gateway
    during a fill are local followers.
- **Models:**

  | Model | Represents |
  |---|---|
  | `one` | `local-one` |
  | `local` | `local-two`: independent gateways |
  | `shared` | A shared store, readable only after a fill completes, so another gateway that misses during the fill fills again |
  | `leased` | `leased-v2`: a fill in flight on any gateway makes the others wait |
  | `shared_ideal` | Leases with instant fills: the floor for any shared tier |

- **Validation:** before any V3.3 run, it reproduced every per-block backend-call
  count that V3.1 and V3.2 measured on `mixed`, for all four modes. For example,
  block 5 predicts 3,081 for local-two and 3,078 for leased; both were measured.

**Harness:**
- `run-v3.py` adds the two workloads and `--keys`, `--zipf-s`, `--hot-keys` and
  `--cache-entries` (recorded in the budget).
- Each run writes `predictions.json` before its first trial.
- **`--restart-trials N`:** for each two-gateway mode, gateway 1 is stopped
  15 s into a 35 s measurement and relaunched at 20 s, with an empty L1. Backend
  calls are recorded in four windows:
  - pre: 5–15 s;
  - down: from the stop until the relaunched gateway is ready;
  - post: the 10 s after it is ready;
  - early: its first 2 s.

  Front-proxy 502s from the stop are reported.
- **`summarize-v3.py`:**
  - sets predicted fills and measured/predicted beside every mode;
  - adds the shared lookup hit ratio column;
  - summarizes restart windows against their predictions.

**Modes:** `leased-v2`, the protocol accepted in V3.2, stands for leases.
`local-two-aa` gives each run its own noise floor.

## Planned runs

```powershell
python bench/run-v3.py --workload zipf   --modes local-one,local-two,shared,leased-v2,local-two-aa --cold-trials 0 --soak 0 --restart-trials 3 --complete
python bench/run-v3.py --workload hotset --max-age 1 --modes local-one,local-two,shared,leased-v2,local-two-aa --cold-trials 0 --soak 0
python bench/run-v3.py --workload mixed  --modes local-one,local-two,shared,leased-v2,local-two-aa --cold-trials 0 --soak 0
```

All three use 5 blocks of 30 s at 500 req/s, 2 gateways, a 50 ms backend, and
`zipf` N = 10,000, s = 1.1 and `hotset` K = 100.
- **hotset:** max-age 1 s gives 30 expiry windows per key per block.
- **Overlap:** each key gets 5 requests/s, so the chance the other gateway asks
  during a 50 ms fill is roughly 1 − e^(−2.5 × 0.05) ≈ 12%. That is not
  negligible, as the plan requires.

## Predictions

Written before the measured runs, from `predict-v3.py` on the exact seeds the
blocks use. Medians are over 5 blocks of 15,000 requests.

| Workload | one | local (local-two) | shared | leased (leased-v2) | shared_ideal | Saving vs local: shared / leased |
|---|---:|---:|---:|---:|---:|---|
| zipf | 3,756 | 4,381 | 3,593 | 3,561 | 3,561 | 18.0% / 18.7% |
| hotset (1 s TTL) | 2,498 | 4,300 | 2,766 | 2,498 | 2,498 | 35.7% / 41.9% |
| mixed (control) | 2,999 | 3,002 | 3,002 | 2,999 | 2,999 | 0.0% / 0.1% |

1. **zipf:**
   - shared and leased-v2 make 18.0% and 18.7% fewer backend calls than
     local-two;
   - the shared lookup hit ratio is above 0;
   - shared is above leased only by the few hot keys that both gateways miss
     inside one fill;
   - local-one (3,756) makes *more* calls than shared, because one 1000-entry
     L1 evicts keys that Redis keeps. This is a capacity effect, not a
     cross-node one.
2. **hotset:**
   - Fills per key-second: local-two 1.43, shared 0.92, leased 0.83.
   - These are below the plan's rough "2 / 1 + overlap / 1", because a key is
     refilled only on its first request after expiry (about 0.2 s later at
     5 req/s per key; about 0.4 s per gateway in local-two).
   - Shared's overlap is 268 extra fills per block (10.7% above leased). That
     is the measured cost of not coordinating.
3. **Restart (zipf, medians of 3 trials, backend calls):**

   | Mode | pre (10 s) | post (10 s) | first 2 s after restart | first 2 s at the steady rate |
   |---|---:|---:|---:|---:|
   | local-two | 1,480 | 1,550 | 388 | 296 |
   | shared | 1,223 | 1,218 | 249 | 245 |
   | leased-v2 | 1,208 | 1,212 | 248 | 242 |

   - **Spike:** the restarted local gateway refills about 92 extra keys in its
     first 2 s (+31%). With Redis, the extra is about 4.
   - **Small over 10 s:** about +5% for local-two. With a 10 s TTL, every L1
     already refills its keys within 10 s, so an empty L1 mostly moves those
     fills forward rather than adding new ones.
   - **Timing:** relaunch takes about 1.3–1.6 s on this host (measured in the
     functional check). The gateway is down about 6.4 s, not the modeled 5 s.
     The down window will run above its prediction, while pre, post and early
     are measured from actual event times.
4. **mixed:**
   - no change from V3.2: local-two and shared about 3,002, local-one and
     leased-v2 3 fewer;
   - shared lookup hit ratio about 0%.
5. **Latency:**
   - shared and leased-v2 p95 stay within about 2 ms of local-two, as on
     `mixed`.
   - On `hotset`, the leased-v2 waiters (about 270 per block) sleep for at least
     one 25–50 ms poll, so leased-v2 p99 may rise above shared's. No other
     latency effect is predicted.
6. **Errors:** no 503s in steady trials. The restart trials show some front
   502s from requests routed to the stopped gateway before the next 100 ms
   probe.

## Results

All three runs ended `SUCCESS`, unpinned, on AC power and on the same binaries:

| Workload | Run | Notes |
|---|---|---|
| zipf | [`20261104-175819-v3`](../../bench/results/v3/20261104-175819-v3/summary.json) | Also ran `--complete` and 3 restart trials per mode. Interrupted once and resumed (see [Standby interruption](#standby-interruption)) |
| hotset | [`20261104-192932-v3`](../../bench/results/v3/20261104-192932-v3/summary.json) | |
| mixed | [`20261104-194601-v3`](../../bench/results/v3/20261104-194601-v3/summary.json) | |

Each run has 5 blocks of 15,000 requests.

### Backend fills against predictions

Medians over 5 blocks. "Measured/predicted" is the range of per-block ratios.

| Workload | Mode | Backend calls | Predicted | Measured/predicted | Backend calls per request | Shared lookup hit ratio | Redis ops per request |
|---|---|---:|---:|---|---:|---:|---:|
| zipf | local-one | 3,755 | 3,756 | 0.998–1.000 | 0.250 | — | 0 |
| | local-two | 4,382 | 4,381 | 1.000–1.002 | 0.292 | — | 0 |
| | shared | 3,594 | 3,593 | 0.999–1.002 | 0.240 | 20.6% | 0.540 |
| | leased-v2 | 3,564 | 3,561 | 1.000–1.001 | 0.238 | 20.9% | 0.541 |
| hotset | local-one | 2,497 | 2,498 | 0.999–1.001 | 0.166 | — | 0 |
| | local-two | 4,295 | 4,300 | 0.999–1.000 | 0.286 | — | 0 |
| | shared | 2,787 | 2,766 | 1.003–1.009 | 0.186 | 42.0% | 0.507 |
| | leased-v2 | 2,494 | 2,498 | 0.997–1.000 | 0.166 | 44.4% | 0.511 |
| mixed | local-one | 2,999 | 2,999 | 1.000 | 0.200 | — | 0 |
| | local-two | 3,002 | 3,002 | 1.000 | 0.200 | — | 0 |
| | shared | 3,002 | 3,002 | 1.000 | 0.200 | 0.0% | 0.400 |
| | leased-v2 | 2,999 | 2,999 | 0.9997–1.000 | 0.200 | 0.1% | 0.400 |

The A/A copy matched local-two within 6 calls per block in every run.

**Backend savings against local-two**, paired in the same block (all beyond the
floor):

| Workload | shared | leased-v2 | Predicted (shared / leased) |
|---|---|---|---|
| zipf | −789 (**−18.0%**) | −818 (**−18.7%**) | −18.0% / −18.7% |
| hotset | −1,508 (**−35.1%**) | −1,797 (**−41.8%**) | −35.7% / −41.9% |
| mixed | 0 | −3 | 0 / −3 |

**Gaps between measured and predicted:**
- **hotset, shared:** 0.3–0.9% above prediction in every block. Measured
  overlap fills (shared − leased) were 293 per block against a predicted 268.
  The model holds a fill for exactly the 50 ms backend delay. A real fill also
  includes the Redis lookup and proxy time, so other gateways have a slightly
  longer window to miss too. leased-v2 turned 271–331 of these overlaps per block
  into waiters.
- **mixed, leased-v2 block 5:** a host stall (536 late arrivals, p99 254 ms)
  caused 4 Redis `timeout` errors, which became 4 coordination 503s. Those
  requests never fetched, so that block made 3,077 calls against 3,078 predicted.
  It was the only error in the three runs' 75 steady trials.
- **local-one on zipf:** it made *more* calls than shared and leased-v2 (3,755
  against 3,594 and 3,564), as predicted. One 1000-entry L1 evicts keys that Redis
  keeps, so leases beat a single gateway by 5.1% here, through capacity rather
  than coordination.

### Latency

Paired against local-two, in ms. `*` marks a median beyond that run's A/A floor.

| Workload | Floor p95 / p99 | shared p95 / p99 | leased-v2 p95 / p99 | leased-v2 − shared p95 / p99 | local-one p95 |
|---|---|---|---|---|---|
| zipf | 0.38 / 0.59 | +1.55\* / +1.90\* | +1.69\* / +2.07\* | +0.26 / +0.59\* | −0.12 |
| hotset | 0.21 / 0.40 | +0.94\* / +1.18\* | +0.98\* / +1.52\* | +0.06 / +0.34 | −0.24\* |
| mixed | 0.47 / 0.42 | +1.62\* / +2.15\* | +1.51\* / +1.93\* | +0.17 / +0.32 | +0.07 |

- **The tier's cost:** the shared tier and leases cost about 1–1.7 ms at p95,
  the Redis round trip on every L1 miss.
- **Leases against shared:** their p95 stayed within the floor on every
  workload. Only on zipf did p99 exceed shared's, by +0.59 ms, which equals
  that run's floor.
- **The predicted hotset p99 rise did not appear:** +0.34 ms, within the
  floor. A waiter's wait (the rest of the owner's fill plus at most one
  25–50 ms poll) is about as long as the duplicate 50 ms fill it replaces in
  shared mode.
- **Medians:** p50 was 0.56–0.67 ms in every mode. There were no steady-trial
  503s, except the 4 in mixed block 5.

### Restart (zipf)

Three trials per mode. Gateway 1 stopped at 14.92–15.00 s and was ready again
at 20.63–21.70 s: relaunch took 0.6–1.7 s after the 20 s mark, so it was down
5.7–6.7 s against the modeled 5 s.

Backend calls, median of 3 trials (predicted in brackets):

| Mode | pre (10 s) | down | post (10 s) | first 2 s | Steady rate per 2 s (pre ÷ 5) | Front 502s (3 trials) |
|---|---:|---:|---:|---:|---:|---:|
| local-two | 1,477 [1,480] | 862 [666] | 1,519 [1,550] | **345** [388] | 295 | 62 |
| shared | 1,228 [1,223] | 785 [609] | 1,230 [1,218] | **249** [249] | 246 | 60 |
| leased-v2 | 1,212 [1,208] | 819 [609] | 1,221 [1,212] | **241** [248] | 242 | 32 |

- **The spike is real only without Redis.** In its first 2 s back, local-two's
  restarted gateway made 323–358 calls against a steady 295 per 2 s. With Redis,
  the same window stayed at the steady rate: shared 209–264, leased-v2 208–246,
  against about 245.
- **It is smaller than predicted, partly because of timing.** By the
  harness's "ready" snapshot, the empty gateway already held 56–81 L1 entries,
  because the front proxy routes to it within one 100 ms probe. In local-two,
  those entries are early-burst fills counted in "down" instead of "first 2 s".
  The longer real downtime also inflates "down" for every mode.
- **Over 10 s the effect is small for every mode**, as predicted. post − pre
  per trial was −29 / +42 / +55 for local-two and −47 / +25 / +19 for shared,
  in line with the predicted −15 / +75 / +95 and −80 / +25 / +24. With a 10 s
  TTL, every L1 refills its keys within 10 s anyway, so an empty L1 mostly moves
  fills earlier.
- **Errors:** 7–30 front 502s per trial. These were requests routed to the
  stopped gateway before the front's next 100 ms probe, plus its in-flight
  requests. There were no transport errors.

### Standby interruption

At 12:20:32 the laptop entered Modern Standby, because of the Balanced plan's
20-minute idle timeout on AC. It stayed there until 13:29:27.
- **Trial affected:** the zipf run's last-but-one trial, local-two restart 3,
  stopped mid-measurement. The harness watchdog marked the run `FAILED`.
- **Trials not affected:** all 32 trials recorded before it finished earlier.
  The hotset and mixed runs started after the system woke, and the System log
  shows no further standby.
- **Fix:** `run-v3.py` now asks Windows to keep the system and display awake
  for the life of the run (`SetThreadExecutionState`; nothing is saved to the
  power settings) and records `kept_awake` in its manifest.
- **Recovery:** the zipf run was resumed with identical settings and binary
  hashes (`resume-1.json`). The interrupted trial was rerun as `attempt-1`,
  and the missing trial and `--complete` checks followed. The original `FAILED`
  marker, partial resource file and logs are kept.

### Checks

- **zipf run's `--complete`:**
  - 10/10 ordinary and 10/10 race test packages;
  - empty vet, build and race-build logs;
  - all ten fault cases under v1, v2 and v2 race;
  - the source-only rebuild.
- **Before the runs:** the new loadgen tests checked:
  - legacy streams unchanged;
  - zipf and hotset streams seeded, ranked and uniform as expected;
  - a scheduled run sends exactly the planned keys, with or without drops.
- **Functional checks** `20261104-175236-v3` (zipf, with one restart trial per
  mode) and `20261104-175603-v3` (hotset) passed. `20261104-175035-v3` failed:
  the relaunched gateway's log was read in place of the generator's. That was
  fixed before any measured run.

## Predictions against results

| # | Prediction | Result | Verdict |
|---|---|---|---|
| 1 | zipf: shared and leased-v2 save 18.0% and 18.7% against local-two; shared hit ratio above 0; local-one above shared | 18.0% and 18.7%; hit ratio 20.6% / 20.9%; local-one 3,755 against 3,594 | Held, to the call: medians within 3 of prediction |
| 2 | hotset: local-two 1.43, shared 0.92, leased 0.83 fills per key-second; shared overlap 268 per block | 1.43 / 0.93 / 0.83; overlap 293 | Held. Overlap was 9% above the model, from fill time beyond the backend delay |
| 3 | Restart: about +92 calls in the first 2 s for local-two, about +4 with Redis; small over 10 s | local-two +28 to +63 (plus about 60 fills before the ready snapshot); shared and leased-v2 −37 to +18; 10 s effects small for all | Direction held; the early-window size was not cleanly measured, because of snapshot timing |
| 4 | mixed: no change from V3.2 | Every block equal to prediction, except one stall-affected block (−1) | Held |
| 5 | Latency: shared and leased-v2 within about 2 ms of local-two at p95; leased-v2 p99 may rise on hotset | +0.9 to +1.7 ms at p95; hotset leased-v2 − shared p99 +0.34, within the floor | Held; the hotset p99 rise did not appear |
| 6 | No steady 503s; some restart 502s | 4 timeout 503s in one stalled mixed block; 7–30 502s per restart trial | Held except one host stall |

## Acceptance

Every acceptance item in the plan is reported above for each workload and
mode:
- backend calls per request;
- shared hit ratio;
- Redis operations per request;
- success, and 503s with causes;
- p50, p95 and p99 against the run's noise floor;
- measured against predicted fills, with each gap explained.

## What this shows

- **Reuse, not the tier, is what saves backend work.** On workloads with
  cross-gateway reuse, the shared tier cut backend calls 18% (zipf) and 35%
  (hotset). Leases cut them 18.7% and 41.8%, all within 1% of a model that
  predicted them from the key stream alone.
- **Leases are worth more as overlap grows.** Their margin over shared was 26
  calls per block on zipf and 289 on hotset, where many keys expire and are
  requested by both gateways within one fill. They cost no measurable p95.
- **The control stays at zero.** On `mixed`, neither tier saves anything, as in
  every run since V2.7.
- **Latency cost:** about 1–1.7 ms at p95 for the Redis round trip on L1 misses.
- **Capacity:** whether these savings raise capacity depends on the backend
  being the bottleneck. On zipf, backend calls per request fell from 0.292 to
  0.238, so a backend-bound ceiling should rise by about 23%. That is V3.4's
  question.
