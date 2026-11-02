#!/usr/bin/env bash
set -euo pipefail
# Start a compiled backend and gateway separately in the desired mode first.
scenario=${1:-hot-key}
url=${2:-http://127.0.0.1:8080}
case "$scenario" in hot-key|mixed) ;; *) echo 'scenario must be hot-key or mixed' >&2; exit 1;; esac
command -v wrk >/dev/null || { echo 'Install wrk before running this script.' >&2; exit 1; }
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
wrk -t4 -c100 -d3s -s "$root/$scenario.lua" "$url"
for trial in 1 2 3; do
  echo "trial=$trial scenario=$scenario"
  wrk -t4 -c100 -d30s --latency -s "$root/$scenario.lua" "$url"
done
