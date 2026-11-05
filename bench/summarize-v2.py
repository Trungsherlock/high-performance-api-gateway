"""Summarize V2 records and fail on inconsistent per-phase accounting."""
import argparse
import json
import statistics
from datetime import datetime, timedelta
from pathlib import Path


def read(path):
    return json.loads(path.read_text(encoding='utf-8-sig'))


def lines(path):
    return [json.loads(line) for line in path.read_text(encoding='utf-8-sig').splitlines() if line.strip()]


def summarize(folder):
    manifest = read(folder / 'manifest.json')
    trials = []
    for path in sorted(folder.glob('*-load.jsonl')):
        trial = path.name.split('-')[0]
        records = lines(path)
        measurement = next(row for row in records if row['Phase'] == 'measurement')
        for row in records:
            assert row['Offered'] == row['Sent'] + row['GeneratorDrops'], (path, 'arrivals')
            assert row['Completed'] == row['Sent'], (path, 'completions')
            assert sum(row['StatusCounts'].values()) == row['Sent'], (path, 'statuses')
            if 'gateway' not in row['Before']:
                continue
            before, after = (row[key]['gateway'] for key in ['Before', 'After'])
            delta = {key: value - before['counters'][key] for key, value in after['counters'].items()}
            assert delta['incoming'] == delta['completed'] == row['Sent'], (path, 'requests', delta)
            assert delta['status_2xx'] == sum(value for key, value in row['StatusCounts'].items() if 200 <= int(key) < 300), (path, 'status class')
            # One rejected shared flight may return 503 to multiple followers.
            for status_class in range(1, 6):
                observed = sum(value for key, value in row['StatusCounts'].items() if int(key) // 100 == status_class)
                assert delta[f'status_{status_class}xx'] == observed, (path, 'status class', status_class)
            if delta['backend_errors'] == 0:
                assert delta['backend_attempts'] == row['After']['backend']['calls'] - row['Before']['backend']['calls'], (path, 'backend')
            if 'permit_queue_full' in delta:
                assert delta['permit_rejections'] == delta['permit_queue_full'] + delta['permit_wait_timeout'], (path, 'rejection reasons')
                reasons = row.get('RejectionReasons', {})
                assert sum(reasons.values()) <= row['Rejected'], (path, 'client rejection reasons')
            if manifest['mode'] == 'full':
                assert delta['cache_hits'] + delta['cache_misses'] + delta['cache_bypass'] == row['Sent'], (path, 'cache')
            for name in ['request', 'upstream', 'permit_wait', 'cache_dependency']:
                hist = after['histograms'][name]
                assert sum(hist['Buckets']) == hist['Count'], (path, 'histogram', name)
            assert after['histograms']['request']['Count'] - before['histograms']['request']['Count'] == row['Sent'], (path, 'request histogram')
            assert all(value == 0 for value in after['gauges'].values()), (path, 'gauge cleanup')
        start = datetime.fromisoformat(measurement['StartUTC'].replace('Z', '+00:00'))
        end = start + timedelta(seconds=measurement['ElapsedSeconds'])
        resources = [row for row in lines(folder / f'{trial}-resources.jsonl') if start <= datetime.fromisoformat(row['time'].replace('Z', '+00:00')) <= end]
        per_process = {}
        for role in ['gateway', 'mockbackend', 'loadgen']:
            samples = [row for row in resources if row['role'] == role]
            if samples:
                q = max(1, len(samples) // 4)
                per_process[role] = {
                    'cpu_seconds_sample_window': samples[-1]['cpuSeconds'] - samples[0]['cpuSeconds'],
                    'max_sampled_rss_mib': max(row['rssBytes'] for row in samples) / 2**20,
                    'first_quarter_median_rss_mib': statistics.median(row['rssBytes'] for row in samples[:q]) / 2**20,
                    'last_quarter_median_rss_mib': statistics.median(row['rssBytes'] for row in samples[-q:]) / 2**20,
                    'samples': len(samples),
                }
        peaks = {}
        metric_path = folder / f'{trial}-metrics.jsonl'
        if metric_path.exists():
            metric_samples = [row for row in lines(metric_path) if start <= datetime.fromisoformat(row['time'].replace('Z', '+00:00')) <= end]
            for name in ['active_backend', 'permit_waiters', 'flights']:
                peaks[name] = max((row['gauges'][name] for row in metric_samples), default=0)
            peaks['cache_entries'] = max((row['cache_entries'] for row in metric_samples), default=0)
            peaks['cache_payload_bytes'] = max((row['cache_payload_bytes'] for row in metric_samples), default=0)
            settings = manifest['gateway']
            limit = settings['maxConcurrent']
            queue = settings.get('maxWaiters', -1)
            if limit > 0:
                assert peaks['active_backend'] <= limit, (path, 'active bound')
                if queue >= 0:
                    assert peaks['permit_waiters'] <= queue, (path, 'queue bound')
                if 'peak' in measurement['After']['backend']:
                    assert measurement['After']['backend']['peak'] <= limit, (path, 'backend peak bound')
        measured_delta = {}
        if 'gateway' in measurement['Before']:
            a, b = measurement['After']['gateway'], measurement['Before']['gateway']
            measured_delta = {key: value - b['counters'][key] for key, value in a['counters'].items()}
        trials.append({'trial': trial, 'successful_rps': measurement['Success'] / measurement['ElapsedSeconds'],
                       'send_p95_ms': measurement['SendLatencyMS'].get('p95'),
                       'rejection_reasons': measurement.get('RejectionReasons', {}),
                       'windows': measurement.get('Windows', {}),
                       'backend_peak': measurement['After']['backend'].get('peak'),
                       'send_p99_ms': measurement['SendLatencyMS'].get('p99'),
                       'scheduled_p99_ms': measurement['ScheduledLatencyMS'].get('p99'),
                       'offered': measurement['Offered'], 'sent': measurement['Sent'],
                       'rejected': measurement['Rejected'], 'transport_errors': measurement['TransportErrors'],
                       'generator_drops': measurement['GeneratorDrops'], 'late_arrivals': measurement['LateArrivals'],
                       'backend_calls': measurement['After']['backend']['calls'] - measurement['Before']['backend']['calls'],
                       'metric_deltas': measured_delta, 'max_sampled_gauges': peaks, 'resources': per_process})
    assert len(trials) == manifest['trials'], (folder, 'missing trials')
    rates = [row['successful_rps'] for row in trials]
    return {'run': folder.name, 'mode': manifest['mode'], 'arrival': manifest['arrival'],
            'seconds': manifest['seconds'], 'trials': trials,
            'successful_rps_median': statistics.median(rates), 'successful_rps_range': [min(rates), max(rates)],
            'send_p99_ms_median': statistics.median(row['send_p99_ms'] for row in trials),
            'scheduled_p99_ms_median': statistics.median(row['scheduled_p99_ms'] for row in trials)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('folders', type=Path, nargs='+')
    args = parser.parse_args()
    print(json.dumps([summarize(folder) for folder in args.folders], indent=2))
