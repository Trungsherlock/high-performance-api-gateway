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
