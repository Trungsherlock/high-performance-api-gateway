# V3 experiment records

| Run | Purpose | Outcome |
|---|---|---|
| [20261104-020914-v3](20261104-020914-v3/summary.json) | V3.0 `--quick --complete` functional check: 2-second trials, tests, race, vet/build, ten fault cases (`faults/`), source-only rebuild | SUCCESS |
| [20261104-021358-v3](20261104-021358-v3/summary.json) | V3.0 count reproduction: one 30-second V2.7-settings block per mode plus cold bursts | SUCCESS; counts within V2.7 ranges, single-trial latency not reproduced |
| [20261104-043055-v3](20261104-043055-v3/summary.json) | V3.1 `--quick` functional check with A/A copy and auto pinning | SUCCESS |
| [20261104-043318-v3](20261104-043318-v3/summary.json) | V3.1 first baseline, `--complete`, auto pinning | SUCCESS, but rejected as baseline: p95/p99 A/A floor 61.75/373.24 ms, 150 front 503s |
| [20261104-045906-v3](20261104-045906-v3/summary.json) | V3.1 pinning test: A/A only, unpinned | SUCCESS; floor 1.02/11.81 ms |
| [20261104-050522-v3](20261104-050522-v3/summary.json) | V3.1 pinning test: A/A only, auto pinning | SUCCESS; floor 6.73/87.69 ms |
| [20261104-051315-v3](20261104-051315-v3/summary.json) | **V3 baseline**: `--complete`, unpinned, 5 modes × 5 blocks, cold bursts, soak, tests, faults, clean rebuild | SUCCESS; floor 0.40/2.63 ms; paired p95 shared +1.93, leased +4.39 ms |
| [20261104-053815-v3](20261104-053815-v3/summary.json) | V3.1 diagnostic: auto layout with backend and front off CPU 0's core | SUCCESS; floor 1.14/53.01 ms, no front 503s |
| [20261104-070718-v3](20261104-070718-v3/summary.json) | `--quick` check that the harness packs logs into `logs.zip` | SUCCESS; 70 files reduced to 14 |
| [20261104-153406-v3](20261104-153406-v3/summary.json) | V3.2 `--quick` functional check with `leased-v2` | SUCCESS; 2.00 ops per fill, cold fill 1 |
| [20261104-153628-v3](20261104-153628-v3/summary.json) | **V3.2 acceptance**: `--complete --trials 6 --soak-mode leased-v2`, 6 modes × 6 blocks, faults under v1, v2 and v2 race | SUCCESS; leased-v2 − leased p95 −2.11 ms, ops per fill 5.00 → 2.00, commands per fill 12.01 → 8.01, 0 503s |
| [20261104-175035-v3](20261104-175035-v3/) | V3.3 `--quick` zipf with a restart trial | FAILED: the harness read the relaunched gateway's log in place of the generator's; fixed |
| [20261104-175236-v3](20261104-175236-v3/summary.json) | V3.3 `--quick` zipf with one restart trial per mode | SUCCESS; 2-second blocks matched predictions exactly |
| [20261104-175603-v3](20261104-175603-v3/summary.json) | V3.3 `--quick` hotset, max-age 1 | SUCCESS |
| [20261104-175819-v3](20261104-175819-v3/summary.json) | **V3.3 zipf**: 5 modes × 5 blocks, 3 restart trials per mode, `--complete` | `FAILED` (Modern Standby mid-trial) then resumed to SUCCESS; shared / leased-v2 −18.0% / −18.7% backend calls |
| [20261104-192932-v3](20261104-192932-v3/summary.json) | **V3.3 hotset**, max-age 1 s | SUCCESS; −35.1% / −41.8% |
| [20261104-194601-v3](20261104-194601-v3/summary.json) | **V3.3 mixed control** | SUCCESS; equal to predictions, 4 timeout 503s in one stalled block |
| [20261105-032930-v3](20261105-032930-v3/summary.json) | V3.4 host probe: `hot`, local-two, 3,000–12,000/s for 10 s each, 50 ms backend | SUCCESS; found the end-of-window generator drops (2 at 3,000/s); drops from 5,000/s |
| [20261105-034409-v34](20261105-034409-v34/summary.json) | V3.4 `--quick` zipf capacity ladder (local-two, shared) | SUCCESS; both fail the lowest rung with 1 s warmups, as predicted |
| [20261105-034519-v34](20261105-034519-v34/summary.json) | V3.4 `--quick` mixed ladder (3 modes), then a replay-only `--resume` | SUCCESS; top of grid reached and confirmed; resume reproduced the summary |
| [20261105-034803-v34](20261105-034803-v34/summary.json) | V3.4 host control, 100 ms backend | SUCCESS, no ceiling; **confounded**: the hot key's 100 ms expiry fill filled the 256 in-flight slots at 2,500/s |
| [20261105-035219-v34](20261105-035219-v34/summary.json) | V3.4 host control, 1 ms backend, from 2,000/s | SUCCESS, no ceiling: host-stall drops in confirmations down to 2,000/s |
| [20261105-035703-v34](20261105-035703-v34/summary.json) | **V3.4 host ceiling**, 1 ms backend, from 1,000/s | SUCCESS; **1,250 req/s** confirmed; every trial from 3,052/s fails |
| [20261105-041050-v34](20261105-041050-v34/summary.json) | **V3.4 mixed capacity**, 250 ms backend, 4 modes | SUCCESS; local-one 472, others 390, all backend-bound |
| [20261105-044743-v34](20261105-044743-v34/summary.json) | **V3.4 zipf capacity**, 250 ms backend, 4 modes, `--complete` | `FAILED` (Redis port collision in WSL), fixed, resumed to SUCCESS; local-two 242, shared and leased-v2 293 (+21%), local-one 293 host-bound |
| [20261105-142924-v3](20261105-142924-v3/summary.json) | **V3.6 final run**: `run-v3.py --complete` from a clean tree at `7957e01`: 6 modes × 5 blocks, cold bursts, soak, tests, faults under v1, v2 and v2 race, clean rebuild | SUCCESS; backend calls equal predictions; v2 − v1 p95 −1.31 ms; floor 0.76 / 3.13 ms; 2 coordination 503s |

Each folder holds:

| File | Contents |
|---|---|
| `manifest.json` | Settings, fixed budget, source and binary hashes; from V3.1, also CPU sets and power state |
| `trials.jsonl` | One line per trial with its raw data, including the generator's output |
| `*-resources.jsonl` | One per trial: per-second CPU, RSS, Redis stats and gateway counters |
| `summary.json` | The reconciled summary |
| `predictions.json` | V3.3+: predicted fills (`run-v3.py`) or predicted ceilings (`capacity-v3.py`), written before the first trial |
| `model-check.json` | V3.4 capacity runs: every trial's own seed replayed through the model, written after the run |
| `tests.txt`, `race.txt`, `vet.txt`, `build.txt`, `faults/`, `clean-source*` | `--complete` checks only |
| `logs.zip` | From V3.1, every per-process log and Redis config (`*.log`, `*.conf`, including those in `faults/` and `clean-source-results/`) |

The summaries need only the manifest, trials and resource files. Process logs
are read only while their trial runs, so `run-v3.py` packs them at the end of
every run, successful or failed, with
[`pack_logs.py`](../../pack_logs.py). The archive is CRC-checked before the
originals are deleted.

The six V3.1 folders listed before the log-packing check were packed afterwards
with the same script. That took them from 1,092 files to 198, and re-summarizing
each from its remaining files reproduced its `summary.json` exactly. The
committed V3.0 folders keep their loose logs.

See [the V3.0 report](../../../docs/v3/README.md), [the V3.1
report](../../../docs/v3/noise-floor.md) and [the final
comparison](../../../docs/v3/final.md).
