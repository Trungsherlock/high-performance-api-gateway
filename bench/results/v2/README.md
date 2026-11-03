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
