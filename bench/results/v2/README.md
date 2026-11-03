# V2 experiment records

Canonical results are enumerated in [summary.json](summary.json) and explained in
[the results report](../../../docs/v2/results.md). Use that explicit run list for
comparisons, rather than globbing every timestamped directory.

Directories retain manifests, available source/binary fingerprints, process
arguments/PIDs, logs, generator phase records, backend counters, and resource and
metric time series. Initial baseline, interrupted, and file-sharing-error runs
are retained as unsuccessful development evidence, not averaged into principal
results. Diagnostic runs cover preserved V1 behavior, generator drops, and an
intentional startup failure. Their acceptance outcomes are described in
[verification](../../../docs/v2/verification.md).

`verification-tests.txt` and `verification-race.txt` record the final test runs.
The original `bench/results/` V1 records outside this directory are unchanged.


V2.2 has a separate [study index](v22-study-20261102-195158.jsonl) and
[summary](v22-study-20261102-195158.summary.json): 15 screening cases plus 24
repeated trials. See [admission results](../../../docs/v2/admission.md).
`v22-tests.txt` and `v22-race.txt` record its checks. V2.0/V2.1 summary records above
remain unchanged; the initial V2.2 smoke failure and corrected smoke are diagnostics.


V2.3 canonical evidence is in [its index](v23-study-20261103.jsonl) and
[18-trial summary](v23-study-20261103.summary.json), with
[interpretation](../../../docs/v2/multi-instance.md). `v23-tests.txt` and
`v23-race.txt` record verification. Earlier V2.3 smoke, failure, and 64-connection
pool runs remain diagnostic records; `v23-initial-steady.summary.json` preserves
the original generator drops and queueing. Do not merge those with final trials.

## V2.4 shared response cache

Use `python bench/run-v24.py --redis-addr 127.0.0.1:6379` with an isolated Redis
and freshly built gateway/backend binaries for the three-mode comparison.
See [V2.4 setup, acceptance checks, and collected results](../../../docs/v2/shared-cache.md).

## V2.5 bounded fill leases

Use `python bench/run-v25.py --redis-addr 127.0.0.1:6379` with an isolated Redis
and fresh gateway/backend/burst binaries. It compares shared and leased bursts
and kills an owner during a fill. See [lease setup, semantics and results](../../../docs/v2/leases.md).

## V2.6 fault injection and degraded modes

`python bench/run-v26.py` owns an isolated Redis and tests outage, delay, OOM,
restart and corrupt data with both policies. `--race` selects a prebuilt
race-instrumented gateway. See [setup, policy semantics and results](../../../docs/v2/faults.md).
