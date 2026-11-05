"""Reconcile a V2.2 study index and produce its machine-readable summary."""
import importlib.util
import json
import sys
from pathlib import Path, PureWindowsPath

spec = importlib.util.spec_from_file_location('v2summary', Path(__file__).with_name('summarize-v2.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
index = Path(sys.argv[1])
records = [json.loads(line) for line in index.read_text(encoding='utf-8-sig').splitlines() if line.strip()]
results = []
for case in records:
    folder = Path(case['folder'])
    if not folder.exists():
        folder = index.parent / PureWindowsPath(case['folder']).name
    summary = module.summarize(folder)
    results.append({'case': case, 'summary': summary})
output = index.with_suffix('.summary.json')
output.write_text(json.dumps(results, indent=2) + '\n', encoding='utf-8')
print(output)
for row in results:
    r, c = row['summary'], row['case']
    ts = r['trials']
    sent = sum(t['sent'] for t in ts)
    rejected = sum(t['rejected'] for t in ts)
    peak_queue = max(t['max_sampled_gauges'].get('permit_waiters', 0) for t in ts)
    rss = max(t['resources'].get('gateway', {}).get('max_sampled_rss_mib', 0) for t in ts)
    print(f"{c['name']}: success RPS={r['successful_rps_median']:.1f}; 503={100*rejected/sent:.2f}%; p99={r['send_p99_ms_median']:.2f}ms; sampled queue={peak_queue}; sampled RSS={rss:.2f}MiB")
