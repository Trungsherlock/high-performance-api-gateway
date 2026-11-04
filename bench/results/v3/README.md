# V3 experiment records

| Run | Purpose | Outcome |
|---|---|---|
| [20261104-020914-v3](20261104-020914-v3/summary.json) | V3.0 `--quick --complete` functional check: 2-second trials, tests, race, vet/build, ten fault cases (`faults/`), source-only rebuild | SUCCESS |
| [20261104-021358-v3](20261104-021358-v3/summary.json) | V3.0 count reproduction: one 30-second V2.7-settings block per mode plus cold bursts | SUCCESS; counts within V2.7 ranges, single-trial latency not reproduced |

Each folder holds the manifest (settings, fixed budget, source and binary
hashes), raw trials, per-process logs, resource samples and a reconciled
summary. See [the V3.0 report](../../../docs/v3/README.md).
