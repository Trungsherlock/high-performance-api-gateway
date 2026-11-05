# V2.7 process resources

Each cell is the median across three trials followed by [min, max]. CPU is seconds
between the first and last samples inside the measured phase; those windows omit
phase edges. RSS is the maximum observed inside each phase, not true peak RSS.
Values are per process, not sums of independently timed maxima or a physical-RAM
accounting. Redis data is from its Linux INFO; the WSL launcher is not substituted.

Host: Intel Core Ultra 7 155H, 22 logical processors, approximately 15.46 GiB RAM,
Windows native Go processes and Redis in WSL2. See the exact hardware manifest.
All processes and observation share host resources; monitoring has cost. Redis
uses the libc/-O1 test build. WSL VM/kernel overhead is not attributed to the
individual process rows, so these are not whole-deployment cost measurements.

| Mode | Process | Sample-window CPU seconds | Max sampled RSS MiB | Sample count |
|---|---|---|---|---|
| local-one | backend | 1.94 [1.80, 2.03] | 15.53 [15.17, 15.75] | 30 [29, 30] |
| local-one | front | 11.86 [11.08, 12.00] | 21.24 [19.52, 23.02] | 30 [29, 30] |
| local-one | gateway-0 | 8.72 [8.50, 9.41] | 21.20 [20.77, 21.36] | 30 [29, 30] |
| local-one | generator | 7.77 [7.52, 7.89] | 22.95 [22.76, 25.29] | 30 [29, 30] |
| local-two | backend | 2.09 [1.28, 2.39] | 15.57 [15.29, 15.85] | 29 [29, 30] |
| local-two | front | 11.62 [10.53, 12.06] | 22.10 [19.50, 22.28] | 29 [29, 30] |
| local-two | gateway-0 | 4.03 [3.78, 4.38] | 18.02 [17.72, 18.89] | 29 [29, 30] |
| local-two | gateway-1 | 4.47 [4.05, 4.92] | 18.19 [17.94, 18.52] | 29 [29, 30] |
| local-two | generator | 7.56 [6.89, 7.80] | 23.42 [22.94, 23.43] | 29 [29, 30] |
| shared | backend | 1.88 [1.72, 2.08] | 15.58 [15.39, 15.99] | 29 [29, 29] |
| shared | front | 11.70 [11.50, 12.12] | 23.18 [21.59, 25.49] | 29 [29, 29] |
| shared | gateway-0 | 5.61 [5.23, 5.83] | 18.73 [18.44, 19.99] | 29 [29, 29] |
| shared | gateway-1 | 5.45 [5.28, 5.83] | 18.95 [18.53, 20.27] | 29 [29, 29] |
| shared | generator | 7.39 [7.02, 7.52] | 24.04 [23.07, 26.74] | 29 [29, 29] |
| shared | redis | 1.30 [1.29, 1.52] | 6.19 [6.02, 6.53] | 29 [29, 29] |
| leased | backend | 1.83 [1.58, 1.86] | 15.98 [15.38, 16.25] | 29 [29, 29] |
| leased | front | 11.61 [11.53, 12.62] | 23.50 [20.41, 23.88] | 29 [29, 29] |
| leased | gateway-0 | 6.55 [5.52, 6.75] | 19.45 [19.03, 19.69] | 29 [29, 29] |
| leased | gateway-1 | 6.45 [6.25, 6.69] | 19.28 [19.25, 20.51] | 29 [29, 29] |
| leased | generator | 7.58 [7.44, 7.72] | 25.14 [24.67, 25.69] | 29 [29, 29] |
| leased | redis | 2.70 [2.54, 2.72] | 6.36 [6.02, 6.53] | 29 [29, 29] |

## Five-minute leased mixed soak

283 samples fell inside the approximately 300-second measured phase. First/last
RSS medians use the first/last 60 seconds; CPU windows span about 299.13 seconds.
These are observed trends, not a claim of flat memory or leak freedom.

| Process | Sample-window CPU seconds | Max sampled RSS MiB | First-60s median RSS MiB | Last-60s median RSS MiB |
|---|---|---|---|---|
| backend | 18.62 | 16.90 | 15.35 | 16.26 |
| front | 119.81 | 27.13 | 22.00 | 23.71 |
| gateway-0 | 62.92 | 21.30 | 18.75 | 20.44 |
| gateway-1 | 63.97 | 20.69 | 18.72 | 19.65 |
| generator | 70.28 | 81.54 | 23.52 | 57.46 |
| redis | 26.32 | 6.36 | 6.36 | 6.36 |


Native process samples come from Windows GetProcessTimes/GetProcessMemoryInfo or
Linux /proc in the portable harness. Redis CPU uses used_cpu_sys + used_cpu_user;
Redis RSS uses used_memory_rss, which is itself periodically refreshed. Generator
RSS includes its retained request samples for exact percentile calculation and is
expected to increase with measurement length; that is separate from gateway cache
retention. No total-service RSS bound or leak-free conclusion is inferred.
