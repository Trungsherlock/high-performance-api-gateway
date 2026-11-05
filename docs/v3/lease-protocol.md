# V3.2 — Lease protocol v2

V3.1 measured leased mode at about +3 ms p95 over shared mode in the same
blocks, at 5 Redis operations per fill against 2. V3.2 removes the three
avoidable round trips. Protocol v1 stays in the same binary and stays the
default until this milestone is accepted, so both protocols run in the same blocks.

## What changed

| Step | v1 (V2.5–V3.1) | v2 |
|---|---|---|
| Miss | Lookup script, then `SET NX PX` | One script reads the response or sets the lease |
| After acquiring | Recheck the response | None: read and acquire were one atomic step |
| Publish | Token-checked script: `SET ... PXAT`, then `DEL` the lease | Unchanged |
| After an acknowledged publish | Token-checked release (always a no-op) | Skipped |
| Fill error, unshareable response, publish error or lost lease | Token-checked release | Unchanged |
| Follower poll | Lookup, then `SET NX` (2 operations) | The same single script (1 operation) |

- **The script** ([sharedcache/lease.go](../../internal/sharedcache/lease.go)) keeps
  the lookup script's size and PTTL checks. It returns:
  - `hit`, with the value and its PTTL;
  - `owner`, after `SET NX PX` with a fresh 256-bit token;
  - `wait`, when another owner holds the lease.
- **Hits** return in that one operation and never take the lease.
- **Unusable values.** When a stored value can't be decoded, the gateway claims
  again with the read skipped. The owner then overwrites the bad value, and
  waiters stop polling as soon as the new value is published, instead of
  polling until the coordination timeout.
- **Selection.** The gateway flag `-lease-protocol v1|v2` defaulted to `v1`
  until acceptance, and now defaults to `v2` (see [Acceptance and
  default](#acceptance-and-default)). `v2` requires a coordinator with the new
  script. A v1-only coordinator is rejected at startup.
- **Unchanged behavior:**
  - degradation policies;
  - fill deadlines, measured from when the acquiring operation started;
  - fail-closed handling of lost leases and ambiguous publications;
  - V1's exact five-step sequence.
- **Limitation:** like v1, this is one Redis authority with expiring ownership
  tokens, not consensus or fencing. The script names the response key and the
  lease key, so a Redis Cluster deployment would need both keys in one hash
  slot. Cluster is out of scope.

## Tests

The existing lease, degradation and publication tests now run under both
protocols: 12 test functions, through a `forProtocols` wrapper. New tests:

| Test | Checks |
|---|---|
| `TestReadOrAcquireIsAtomic` (real Redis) | 20 concurrent claims give exactly 1 owner and 19 waits. After publish, a claim is a hit and creates no lease. |
| `TestReadOrAcquireUnusableValues` (real Redis) | A corrupt value is reported unusable without taking the lease, then replaced after a claim that skips the read. An oversize value is treated as a miss. |
| `TestStaleClaimOwnerCannotPublishOrRelease` (real Redis) | An expired v2 owner can neither release nor publish over its successor. |
| `TestLeaseOperationsPerFill` (real Redis, both protocols) | A fill costs 5 operations under v1 and 2 under v2, with no release after publish. A second gateway reads the published value in 1 operation. |
| `TestLeaseV2CorruptValueRefilledWithoutStall` (real Redis) | 20 requests across 2 gateways with a corrupt stored value: 1 backend fill, and the waiter finishes within 500 ms of publication (wait bound 2 s). |
| `TestLeaseRecoversAfterAbandonedLease` (real Redis, both protocols) | After an owner dies holding a 150 ms lease, the next request waits it out and fills once. |
| `TestLeaseProtocolValidation` | Unknown protocols and v2 with a v1-only coordinator are rejected. |

Before the measured run, these passed against a harness-built Redis 7.4.2, in
both the ordinary and race suites.

## Harness changes

- **`run-v3.py`:**
  - Adds a `leased-v2` mode. `leased` passes `-lease-protocol v1`
    explicitly.
  - `--complete` builds `bin/gateway-race`, then runs the ten V2.6 fault cases
    three times: v1 (regression), v2, and v2 with the race-detector gateway,
    into `faults/v1`, `faults/v2` and `faults/v2-race`.
- **`run-v26.py`:** gains `--lease-protocol`. Its default is `v1`.
- **`summarize-v3.py`:**
  - Adds `cmd/fill`: Redis `total_commands_processed` per backend fill,
    measured inside the sampled measurement window, net of the sampler's own
    `INFO` calls.
  - Adds same-block contrasts: `leased-v2 − leased`, `leased-v2 − shared`
    and `leased − shared`, judged against the run's A/A floor.
  - Adds a total-rejections column.

## Predictions

Written before the measured run. The basis is the V3.1 baseline
[`20261104-051315-v3`](../../bench/results/v3/20261104-051315-v3/summary.json),
re-read with the new summarizer columns:
- shared costs 3.00 Redis commands per fill and leased 12.01;
- in the same blocks, leased − shared p95 was +2.96 ms (all 5 blocks positive),
  and p99 +3.54 ms.

1. **Operations per fill:** leased 5.0, leased-v2 2.0, the same as shared.
2. **Redis commands per fill: 12 → 8, a 33% drop.** The plan predicted about
   60%, which assumed one command per operation. V3.1's numbers show Redis also
   counts the commands a script runs:
   - shared: lookup `EVAL` + `STRLEN`, then `SET` = 3;
   - v1: lookup 2, `SET NX` 1, recheck 2, publish 5 (`EVAL`, `GET`, `TIME`,
     `SET`, `DEL`), release 2 = 12;
   - v2: `EVAL` + `STRLEN` + `SET` = 3, plus publish 5 = 8.
3. **Latency:**
   - leased-v2 − leased: about −2.5 to −3.5 ms at p95, beyond the floor;
     negative at p99.
   - leased-v2 − shared: within the run's A/A floor at p95.
4. **Backend work is unchanged:**
   - leased-v2 makes the same backend calls as leased, 3 fewer than local-two
     per block;
   - 3 distributed followers per block;
   - 1 fill per cold burst.
5. **Errors:**
   - zero coordination 503s in most blocks;
   - a host stall may still cause a few Redis `timeout` errors (V3.1 had 2 in
     one block);
   - every 503 is reported with its cause.
6. **Faults:** all ten cases pass under v2 with ordinary and race builds, and
   still pass under v1.
7. **Soak** (leased-v2, 5 minutes):
   - about 150,000 successes;
   - 2.00 operations per fill;
   - memory flat, as in V3.1.

## Run

```powershell
python bench/run-v3.py --complete --trials 6 --soak-mode leased-v2
```

Six modes and six blocks, so every mode runs once in every position. The soak
uses the new protocol.

## Results

**Later result:** V3.6's final run measured a smaller gain over v1: 1.31 ms at p95,
with the p99 gain inside that run's floor. See
[the final comparison](final.md#lease-protocol-v1-against-v2).

Run [`20261104-153628-v3`](../../bench/results/v3/20261104-153628-v3/summary.json)
passed all its checks (`SUCCESS`). It ran on the working tree above `af500af`,
unpinned, on AC power. The functional check before it,
[`20261104-153406-v3`](../../bench/results/v3/20261104-153406-v3/summary.json)
(`--quick`), also passed.

### Cost and latency

Medians over 6 blocks at 500 req/s, `mixed` workload, 2 gateways.

| Mode | Send p95 ms | Send p99 ms | Backend calls | Ops per fill | Redis commands per fill | 503s | Dependency errors |
|---|---:|---:|---:|---:|---:|---:|---:|
| local-one | 51.60 | 52.22 | 2,986 | 0.00 | — | 0 | 0 |
| local-two | 51.66 | 52.24 | 2,989 | 0.00 | — | 0 | 0 |
| shared | 53.11 | 54.75 | 2,989 | 2.00 | 3.00 | 0 | 0 |
| leased (v1) | 55.03 | 57.88 | 2,986 | 5.00 | 12.01 | 0 | 0 |
| **leased-v2** | **53.15** | **54.70** | 2,986 | **2.00** | **8.01** | 0 | 0 |
| local-two-aa | 51.71 | 52.36 | 2,989 | 0.00 | — | 0 | 0 |

**Noise floor:** the largest A/A difference over 6 blocks was 0.15 ms at p50,
0.72 ms at p95, 2.03 ms at p99 and 2.28 ms at scheduled p99. Backend calls
differed by 0.

Same-block contrasts. Each cell is the median [min, max] over 6 blocks;
`*` marks a median beyond the floor.

| Contrast | Send p95 ms | Send p99 ms | Scheduled p99 ms | Blocks where the first mode was slower (p95) |
|---|---|---|---|---|
| leased-v2 − leased | **−2.11\*** [−2.95, −1.25] | **−3.77\*** [−6.48, −1.11] | **−3.98\*** [−6.63, −1.02] | 0 of 6 |
| leased-v2 − shared | +0.07 [−0.80, +0.35] | −0.05 [−2.29, +1.13] | −0.00 [−2.47, +1.15] | 4 of 6 |
| leased − shared | +1.96\* [+1.35, +2.58] | +3.19\* [+0.92, +4.61] | +3.36\* [+0.92, +4.69] | 6 of 6 |

Against `local-two`, paired p95 was +1.69\* ms for shared, +3.59\* for leased v1
and +1.42\* for leased-v2. local-one was −0.01 ms, within noise.

**What changed:**
- Protocol v2 made the lease path **2.1 ms faster at p95 and 3.8 ms at p99**
  than v1, in every block.
- It brought leased mode to within the noise floor of shared mode: +0.07 ms
  p95 against a 0.72 ms floor. In this run, v1's whole p95 gap to shared was
  1.96 ms, and v2 removed all of it.
- 3 fewer synchronous operations per fill saved about 0.7 ms of p95 each.
- Redis commands per fill fell from 12.01 to 8.01 (−33%), consistently in
  every block (8.00–8.02).
- Operations per fill fell from 5.00 to 2.00, the same as shared.

**Backend work did not change.** leased-v2 made exactly the same backend calls
as leased in every block (2,901–3,078), 3 fewer than local-two and shared. It had
3 distributed followers per block, and every cold burst made 1 backend fill
(3 of 3).

**Errors:** none in the 36 steady trials. There were no 503s, dependency errors,
generator drops or front-proxy unavailability.

### Soak (leased-v2, 5 minutes)

| Measure | Result |
|---|---|
| Requests | 150,000 sent; **150,000 successes** |
| Rejections and errors | 0 rejections, 0 dependency errors, 0 generator drops |
| Send p50 / p95 / p99 | 0.63 / 53.21 / 55.44 ms |
| Backend calls | 30,031 |
| Ops per fill | 2.003 |
| Redis commands per fill | 8.01 |
| Distributed followers | 30 |
| Gateway RSS (early → late median) | 18.2 → 19.6 MiB and 18.2 → 19.8 MiB |

The V3.1 soak, which used v1, had p99 59.66 ms. These are different runs, so
that difference is not a paired result.

### Faults, tests and rebuild

- **Fault cases:** all ten passed in each configuration:
  - [`faults/v1`](../../bench/results/v3/20261104-153628-v3/faults/v1/20261104-160808-v26/summary.json): protocol v1, ordinary build;
  - [`faults/v2`](../../bench/results/v3/20261104-153628-v3/faults/v2/20261104-160852-v26/summary.json): protocol v2, ordinary build;
  - [`faults/v2-race`](../../bench/results/v3/20261104-153628-v3/faults/v2-race/20261104-160931-v26/summary.json): protocol v2, race-detector build.

  Under both protocols, each fault recorded the same error cause: outage and
  restart `connection`, `CLIENT PAUSE` `timeout`, OOM `other`, and a corrupt value
  none. Every case's causes summed to its dependency errors. The corrupt-value
  case refilled with exactly one backend call under v2. Error counts per case
  vary with fault timing from run to run, as they did in V3.0.
- **Tests:** ordinary and race suites passed against harness-owned Redis (10 of
  10 packages each). Vet, build and the race-gateway build logs were empty.
- **Rebuild:** the source-only rebuild passed.

## Predictions against results

| # | Prediction | Result | Verdict |
|---|---|---|---|
| 1 | Ops per fill: leased 5.0, leased-v2 2.0, same as shared | 5.00 / 2.00 / 2.00 | Held |
| 2 | Redis commands per fill 12 → 8 (−33%); the plan's −60% assumed one command per operation | 12.01 → 8.01 (−33.3%) | Held. The plan's original −60% was wrong, as stated before the run |
| 3a | leased-v2 − leased p95 −2.5 to −3.5 ms beyond the floor; p99 negative | p95 −2.11\* (6 of 6 blocks); p99 −3.77\* | Direction and significance held; p95 magnitude fell below the range. The v1 gap to shared was also smaller in this run (1.96 ms against 2.96 ms in V3.1), and v2 closed all of it |
| 3b | leased-v2 − shared p95 within the floor | +0.07 ms against a 0.72 ms floor | Held |
| 4 | Same backend calls as leased; 3 distributed followers per block; 1 cold fill | Identical calls in every block; 3 per block; 1/1/1 | Held |
| 5 | Zero coordination 503s in most blocks | Zero in all 36 steady trials and in the soak | Held |
| 6 | Ten fault cases pass under v2 ordinary and race, and still under v1 | All 30 passed | Held |
| 7 | Soak: about 150,000 successes, 2.00 ops per fill, flat memory | 150,000/150,000; 2.003; 18.2 → 19.7 MiB | Held |

## Acceptance and default

The plan's acceptance criteria are met:
- the four-mode comparison was rerun with both protocols in the same blocks;
- ops per fill, paired p95/p99 differences and 503 counts with causes are
  reported above;
- the p95 gap shrank to within the floor, as predicted.

**Default switched to v2.** After this run, the gateway's `-lease-protocol`
default changed to `v2`, as the plan specified on acceptance.
- `v1` remains selectable.
- The `leased` harness mode still passes `v1` explicitly, so comparisons are
  unchanged.
- `run-v3.py --soak-mode` now defaults to `leased-v2`.
- The `LeaseOptions` zero value in the library still means v1, for
  compatibility.
- The acceptance run used binaries built before the switch. That makes no
  difference, because every mode passed its protocol explicitly.

## What V3.2 does not change

On `mixed`, leases still save only the 3 synchronized hot-key expiries per
30-second block, and shared storage saves no backend calls. V3.2 removed the
lease *cost*; it did not create a *benefit*. That needs workloads with
cross-node reuse, which is V3.3.
