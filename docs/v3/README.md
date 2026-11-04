# V3.0 — Frozen V2 baseline and a configurable harness

V3.0 prepares the measurements the [V3 plan](../../high_performance_api_gateway_v3_plan.md)
needs. It does not change caching, coalescing, admission or lease behavior.

## Baseline

- V2.7 is tagged `v2-final` at `66c9ccd`. V1 and V2 artifacts are unchanged.
- V3 artifacts go under `bench/results/v3/`; V3 reports go under `docs/v3/`.
- Commit `92a0ad7` keeps its `\` message. Rewording it would rewrite `66c9ccd`,
  which two branches already point at.

## Run

```powershell
# Defaults reproduce the V2.7 study: 3 trials per mode, cold bursts, 5-minute soak.
python bench/run-v3.py --complete

# Functional check: short trials, then tests, race tests, faults and a source-only rebuild.
python bench/run-v3.py --quick --complete

# Example selection: two modes, two offered rates, five blocks, no soak.
python bench/run-v3.py --modes shared,leased --rates 500,1000 --trials 5 --soak 0

# Re-check an artifact's accounting and print its table.
python bench/summarize-v3.py bench/results/v3/<run>
```

| Option | Default | Meaning |
|---|---|---|
| `--modes` | `local-one,local-two,shared,leased` | Modes to compare, rotated per trial block |
| `--workload` | `mixed` | Generator workload: `hot`, `mixed` or `unique` |
| `--rates` | `500` | Scheduled arrivals/sec; every rate gets its own trial blocks |
| `--gateways` | `2` | Gateways for every mode except `local-one`: 2, 4 or 8 |
| `--delay` / `--cold-delay` | `50` / `500` | Backend delay in ms for steady and cold-burst trials |
| `--max-age` | `10` | Backend `Cache-Control` max-age; gateways cap lifetimes at 10 s |
| `--concurrency` | `256` | Generator in-flight requests and connections |
| `--seconds` / `--warmup` / `--trials` | `30` / `5` / `3` | Steady-trial shape |
| `--cold-trials` / `--soak` / `--soak-mode` | `3` / `300` / `leased` | Cold bursts and the soak |
| `--resume <run>` | — | Continue an interrupted run with identical settings and hashes |

These stay fixed across runs and are recorded under `budget` in each manifest:
- aggregate 32 backend permits, 32 queue slots, 64 cache flights and 1,024
  cache-miss clients, split evenly across gateways;
- 1,000 entries and 16 MiB of L1 per gateway;
- a 50 ms Redis operation deadline, and 64 MiB of Redis memory with `noeviction`.

`--gateways` is limited to 2, 4 or 8 so those budgets divide evenly. `local-two`
keeps its V2.7 name and means independent local-only gateways, however many
`--gateways` selects. Cold bursts assert one backend fill for `local-one` and
`leased`, and one fill per gateway for `local-two` and `shared`.

`--complete` does three things:
- reuses the V2.7 ordinary, race, vet and build checks;
- runs the ten V2.6 fault cases into the run's `faults/` folder (`run-v26.py`
  gained `--results`; its default is still `bench/results/v2`);
- rebuilds a source-only snapshot and smoke-tests it with the same topology and workload.

## Dependency-error causes

Every `dependency_errors` increment now also increments exactly one fixed counter:

| Counter | Meaning |
|---|---|
| `dependency_error_pool_timeout` | The operation deadline expired while waiting for a pooled connection. No command was sent. |
| `dependency_error_timeout` | A deadline or network timeout after a connection was obtained |
| `dependency_error_connection` | Closed client or connection, EOF, refused or reset dials, and other non-timeout network errors |
| `dependency_error_other` | Redis error replies such as OOM, malformed replies and canceled contexts |

go-redis v9.7.3 keeps its pool-timeout error internal. Its pool wait (50 ms)
also equals the operation deadline, which starts first. Without a change, a
saturated pool would have looked like an ordinary timeout. The adapter
([redis.go](../../internal/sharedcache/redis.go)) now reserves one slot per pool
connection before each command, inside the same deadline. Because the slot count
equals the pool size, go-redis's own pool wait never blocks. Pool size, deadline
and retry policy are unchanged.

Tests cover the cause mapping, a saturated pool returning `ErrPoolWait` from all
five operations without dialing, and slots released after failed operations.
Gateway tests check that the causes sum to `dependency_errors`, including for
unavailable stores, acquisition deadlines and both degradation policies.

## Derived summary columns

[summarize-v3.py](../../bench/summarize-v3.py) keeps every V2.7 reconciliation
check and adds:

- `dependency_ops_per_fill`: cache-dependency operations ÷ backend calls. Each
  backend call is one fill in these eligible-GET workloads.
- `dependency_ops_per_request`: cache-dependency operations ÷ client requests sent.
- `backend_calls_per_request`.
- `dependency_error_causes` per trial, asserted to sum to `dependency_errors`.

It prints medians per rate and mode, plus cold-burst fills, after reconciliation.

## Acceptance evidence

Both runs used the current working tree, which is uncommitted above `66c9ccd`.
Source and binary hashes are in each manifest.

**Functional check:** [`20261104-020914-v3`](../../bench/results/v3/20261104-020914-v3/summary.json),
`--quick --complete`.
- All 8 trials reconciled. Cold fills were 1/2/2/1.
- [Ordinary](../../bench/results/v3/20261104-020914-v3/tests.txt) and
  [race](../../bench/results/v3/20261104-020914-v3/race.txt) tests passed against
  the harness-owned Redis. Vet and build were clean.
- The [ten fault cases](../../bench/results/v3/20261104-020914-v3/faults/20261104-021122-v26/summary.json) passed.
- The [source-only rebuild](../../bench/results/v3/20261104-020914-v3/clean-source.json)
  passed its eight smoke trials.

The fault cases gave a real-Redis check of the new counters. In every case the
causes summed to `dependency_errors`:

| Fault | protection-first | available-local | Cause recorded |
|---|---:|---:|---|
| Redis stopped | 12 | 2 | connection |
| `CLIENT PAUSE` 350 ms | 2 | 2 | timeout |
| `maxmemory` OOM | 16 | 2 | other |
| Redis restarted (stale pooled connection) | 1 | 1 | connection |
| Corrupt value | 0 | 0 | — (treated as a miss) |

No pool timeouts occurred. Pool saturation is covered by unit tests.

**Count reproduction:** [`20261104-021358-v3`](../../bench/results/v3/20261104-021358-v3/summary.json),
`--trials 1 --cold-trials 1 --soak 0`. This is one 30-second block per mode with
V2.7 defaults. The quick check's 2-second trials cannot reproduce 30-second counts.

| Mode | Backend calls (V2.7 range) | Dependency operations (V2.7 range) | Ops per fill | Send p95 / p99 ms (V2.7 p95, p99 ranges) |
|---|---|---|---:|---|
| local-one | 2,973 (2,969–3,034) | 0 | 0.00 | 65.66 / 153.80 (53.52–56.31, 69.75–89.27) |
| local-two | 2,976 (2,972–3,037) | 0 | 0.00 | 51.68 / 53.09 (52.21–54.40, 60.48–71.08) |
| shared | 2,976 (2,972–3,037) | 5,952 (5,944–6,074) | 2.00 | 54.73 / 59.96 (55.35–58.78, 63.42–84.39) |
| leased | 2,973 (2,968–3,034) | 14,880 (14,855–15,185) | 5.01 | 56.83 / 64.65 (59.55–63.11, 71.76–111.05) |

Every mode sent all 15,000 requests with no drops, 503s, transport errors or
dependency errors. Leased mode had 3 distributed followers, as in V2.7. Cold
fills were 1/2/2/1. Backend calls, dependency operations and operations per fill
all fall inside V2.7's ranges, which meets V3.0's acceptance.

Latency did not reproduce V2.7's ranges, in either direction:
- **local-one** ran first in the block, right after the build, and was far
  slower: 939 late arrivals and a scheduled p99 of 168 ms. It does not use Redis,
  and its counts matched, so this is not attributed to the V3.0 change. The cause
  was not established.
- **The other three modes** were below their V2.7 ranges. With one trial and no
  measured noise floor, neither result is an effect.

V3.1 adds the A/A noise floor and more blocks. Use the counts above as V3.0's
reproduction claim, not the latencies.

L1 request hit ratios were 79.87, 79.81, 79.77 and 79.66%. local-one and
local-two were up to 0.08 points above V2.7's upper bounds.
