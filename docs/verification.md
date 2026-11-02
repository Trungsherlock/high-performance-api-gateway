# Final verification — Milestone 5

Verified on 2026-11-02 with Go 1.25.7, native Windows/amd64.

## Clean-source verification

Initialized the local Git repository and exported the staged source to a new
temporary directory using `git checkout-index`. The export contained the source,
tests, documentation, and recorded experiment data, but no existing `bin`
executables, ignored profiles, or local build-cache directories. Go's shared build
cache remained available; this was a clean source export, not a cold compiler cache.

Ran these commands from that export:

```powershell
$env:Path = 'C:\msys64\mingw64\bin;' + $env:Path
go test ./... -timeout 60s
go test -race ./... -timeout 60s
go vet ./...
go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/bench.exe ./cmd/bench
```

All commands passed. All test-bearing packages executed successfully with the
race detector. The benchmark and mock-backend command packages have no command-
level tests; their behavior is exercised through the harness and backend tests.

Built backend/gateway processes were started on loopback ports 19000/18080 to
avoid conflicting with the documented default ports. Two identical public item
requests returned the expected JSON, and the fresh backend reported one data
call. The processes were stopped after the smoke check. Bounded shutdown is also
covered by the gateway shutdown test.

## Resolved Windows race-build failure

Previously `go test -race` failed with `runtime/cgo ... exit status 2`.
`gcc --version` succeeded, but a minimal C compile failed when GCC's helper
programs could not find their runtime dependencies. Prepending
`C:\msys64\mingw64\bin` to the current process's PATH made the compile succeed
and unblocked all package race tests.

This was a process-local environment adjustment; no global PATH or Go settings
were changed. On another machine, use the directory of its compatible compiler
installation. Passing tests exercise the covered paths; they do not prove the
absence of every possible concurrency defect.

## Benchmark reproduction from the clean export

```powershell
./bin/bench.exe -mode full -scenario cold -trials 3
./bin/bench.exe -mode full -scenario hot -duration 2s -warmup 300ms -trials 3
```

- Cold: all three trials served 100 successful requests with exactly one backend
  call, zero errors, and zero rejections.
- Hot: all three trials had zero measured backend calls, errors, or rejections.
  Successful throughput ranged from about 32,502 to 49,593 requests/sec, with a
  median around 36,105. This separate reproduction is not substituted for the
  earlier Milestone 4 comparison and confirms the timing variability noted there.
- Raw reproduction records are retained in
  [milestone5-cold.jsonl](../bench/results/milestone5-cold.jsonl) and
  [milestone5-hot.jsonl](../bench/results/milestone5-hot.jsonl).

The native harness still shares one process/runtime with the servers. wrk
execution, long-duration soak testing, and production deployment were outside
this verification. See [results](results.md) for the original experiment limits.

## Delivery

- README includes compiled-binary setup, request-flow diagram, policy, limits,
  baseline/full-mode commands, and the Windows race-test fix.
- [Design notes](design.md) explain the mutex decision, stampede prevention,
  cancellation ownership, overload/error tradeoffs, and HTTP limitations.
- Git attributes preserve LF for Go, Lua, and shell source across checkouts.
- Build executables, test binaries, cache directories, and raw profiles are ignored.
- Source and evidence are committed locally; no remote publication is required
  for this milestone.
