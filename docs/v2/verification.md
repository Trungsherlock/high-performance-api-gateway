# V2.0-V2.1 verification

Recorded 2026-11-02 (America/Chicago). The original V1 plan and result files were
preserved. `v1-baseline` points to a567362; only documentation differs from the
V1 implementation commit 465d9f4. Work remains on the existing V2 branch.

## Checks executed

```powershell
$env:Path = 'C:\msys64\mingw64\bin;' + $env:Path
go test ./...
go test -race ./... -timeout 60s
go vet ./...
go build ./...
gofmt -l cmd internal
git diff --check
```

All passed; formatting produced no file paths. Ordinary and race output is retained
in `bench/results/v2/verification-tests.txt` and `verification-race.txt`. The compiler
PATH change was terminal-local. No remote publication or V2 implementation commit
was performed.

New tests exercise deterministic hit/miss/follower/backend reconciliation,
shared rejection fanout (one failed admission, twenty client 503s), permit-waiter
cancellation and rejection cleanup, backend gauges through body completion,
request abort accounting, safe log fields, blocked-sink log drops, scheduled
arrival saturation, closed-loop concurrency, and distinct transport failures.
Existing cache/proxy/coalescing/limiter/shutdown tests remain passing.

## Separate-process evidence

- Twelve principal steady trials: each combination of scheduled/closed arrivals
  and proxy/full modes has three 30-second measurements with 5-second warmup.
- One full-mode 300-second soak, with 150,000 successful measured requests.
- All canonical records passed `bench/summarize-v2.py` reconciliation of offered,
  sent, dropped, completed, client statuses, backend counters, cache accounting,
  histogram totals, and drained active-work gauges.
- Summaries and exact run IDs are in [results](results.md) and
  [summary.json](../../bench/results/v2/summary.json).

Additional diagnostics (each in its own timestamped directory):

| Run suffix | Measured result |
|---|---|
| `193515-690-verified-baseline-proxy` | 1,000/1,000 successful; 1,000 backend calls |
| `193535-900-verified-baseline-full` | 1,000/1,000 successful; 189 backend calls |
| `193548-234-verified-generator-drops` | 1,000 offered; 19 sent/successful; 981 generator drops; zero client 503s |
| `193609-048-expected-startup-failure` | Negative backend delay caused startup failure; nonzero harness exit and captured failure record |

The baseline diagnostics compiled unchanged V1 servers from the tagged worktree
and used the current external generator. The drop diagnostic used proxy mode,
1,000 arrivals/sec, one generator slot, and 50 ms backend delay. It demonstrates
that unsent generator work is counted independently from gateway rejection.
After intentional startup failure, process inspection found no gateway, backend,
or generator children remaining. This checks the normal error-cleanup path;
it does not guarantee cleanup if the host process or machine is forcibly killed.

Running `bin/gateway.exe -admin-addr 0.0.0.0:0` exited with code 1 and the expected
numeric-loopback validation error. Public health and dedicated admin readiness
were exercised by the harness. Actual readiness here means local initialization,
not a successful backend dependency probe.

Earlier interrupted runs and harness failures remain on disk and are excluded
from canonical results. The initial exit-code bug and Windows append sharing error
are documented in the result report. The analyzer originally expected rejected
admissions to equal client 503s; shared failure fanout required distinct semantics
and now has a deterministic test.

## Remaining scope

Five minutes of bounded observed cache cardinality and successful cleanup is not
a proof of leak-free long-term operation. Gateway RSS increased modestly; the
exact-percentile generator retains samples and has greater intentional growth.
Permit wait duration is bounded, but gateway waiter count remains unbounded.
Shared cache/dependency metrics are reserved at zero because Redis and later V2
milestones have not been implemented.
