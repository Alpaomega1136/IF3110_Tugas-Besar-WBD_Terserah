"""Convert k6 JSON points + raw event log into a reproducible Markdown report."""
import argparse
import json
import re
from collections import defaultdict
from datetime import datetime
from itertools import combinations
from pathlib import Path


def percentile(values, q):
    if not values:
        return None
    values = sorted(values)
    pos = (len(values) - 1) * q
    i = int(pos)
    return values[i] + (values[min(i + 1, len(values) - 1)] - values[i]) * (pos - i)


def fmt(value):
    return 'N/A' if value is None else f'{value:.2f}'


def timing(values):
    return f'{fmt(percentile(values, .5))} / {fmt(percentile(values, .95))}'


def stamp(value):
    value = value.replace('Z', '+00:00')
    value = re.sub(
        r'\.(\d+)',
        lambda m: '.' + m.group(1).ljust(6, '0')[:6],
        value
    )
    return datetime.fromisoformat(value)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('points', type=Path)
    parser.add_argument('events', type=Path)
    parser.add_argument('report', type=Path)
    args = parser.parse_args()
    profile, jobs, audit = None, [], []
    for line in args.events.read_text(encoding='utf-8-sig', errors='replace').splitlines():
        if line.startswith('K6_PROFILE '):
            profile = json.loads(line.removeprefix('K6_PROFILE '))
        elif line.startswith('K6_JOB '):
            jobs.append(json.loads(line.removeprefix('K6_JOB ')))
        elif line.startswith(('K6_AUDIT ', 'K6_JOB_ERROR ')):
            audit.append(line)
    if profile is None:
        raise SystemExit('Missing K6_PROFILE event: check setup and raw log format')
    by_phase = defaultdict(lambda: defaultdict(list))
    by_cohort = defaultdict(lambda: defaultdict(list))
    accepted_metrics = {'http_req_duration', 'http_reqs', 'http_req_failed', 'checks'}
    with args.points.open(encoding='utf-8-sig') as f:
        for line in f:
            record = json.loads(line)
            metric = record.get('metric', '')
            if record.get('type') != 'Point' or not (metric in accepted_metrics or metric.startswith('stress_')):
                continue
            data = record['data']
            tags = data.get('tags') or {}
            phase = tags.get('phase', 'untagged')
            by_phase[phase][metric].append(data['value'])
            if tags.get('cohort'):
                by_cohort[tags['cohort']][metric].append(data['value'])
    lines = [f'# k6 stress test — {profile["profile"]}', '', f'Run ID: `{profile["runId"]}`. Planned jobs: {profile["totalJobs"]}.', '',
             '## HTTP workload', '', '| Level | API VU target | Window (s) | Requests | req/s | p50 (ms) | p95 (ms) | HTTP error (%) | Threshold |',
             '|---|---:|---:|---:|---:|---:|---:|---:|---|']
    for p in profile['phases']:
        m = by_phase[p['name']]
        req = int(sum(m['http_reqs']))
        errors = m['http_req_failed']
        rate = sum(errors) / len(errors) * 100 if errors else None
        p50, p95 = (percentile(m['http_req_duration'], q) for q in (.5, .95))
        threshold = 'N/A' if p95 is None or rate is None else ('PASS' if p95 < 1000 and rate < 5 else 'FAIL')
        lines.append(f'| {p["name"]} | {p["vus"]} | {p["duration"]} | {req} | {fmt(req/p["duration"])} | {fmt(p50)} | {fmt(p95)} | {fmt(rate)} | {threshold} |')
    lines.extend(['', '## Migration throughput by observation phase', '',
                  '| Level | Attempted | Accepted | Terminal observed | Success observed | Non-success | Unresolved events | Terminal/s | Success/s | Queue p50/p95 (ms) | Duration p50/p95 (ms) |',
                  '|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|'])
    for p in [*profile['phases'], {'name': 'drain', 'duration': None}]:
        m = by_phase[p['name']]
        a, b, c, d, e, u = (int(sum(m['stress_jobs_'+key])) for key in ('attempted','accepted','terminal','success','unsuccessful','unresolved'))
        sec = p['duration']
        lines.append(f'| {p["name"]} | {a} | {b} | {c} | {d} | {e} | {u} | {fmt(c/sec if sec else None)} | {fmt(d/sec if sec else None)} | {timing(m["stress_queue_wait_ms"])} | {timing(m["stress_job_duration_ms"])} |')
    lines.extend(['', '## Jobs by submission cohort', '', '| Level | Planned A/B | Accepted | Terminal | Success | Retries | Queue p50/p95 (ms) | Duration p50/p95 (ms) |', '|---|---|---:|---:|---:|---:|---|---|'])
    for p in profile['phases']:
        m = by_cohort[p['name']]
        a,b,c,r = (int(sum(m['stress_'+k])) for k in ('jobs_accepted','jobs_terminal','jobs_success','job_retries'))
        lines.append(f'| {p["name"]} | {p["a"]}/{p["b"]} | {a} | {b} | {c} | {r} | {timing(m["stress_queue_wait_ms"])} | {timing(m["stress_job_duration_ms"])} |')
    # Use actual backend timestamps rather than client polling timestamps to check overlap.
    good = [j for j in jobs if j['status'] == 'success' and j['started_at'] and j['finished_at'] and j['attempts'] == 1]
    overlaps = defaultdict(list)
    for a,b in combinations(good,2):
        if max(stamp(a['started_at']), stamp(b['started_at'])) < min(stamp(a['finished_at']), stamp(b['finished_at'])):
            overlaps['same' if a['target'] == b['target'] else 'different'].append((a['job'], b['job']))
    lines.extend(['', '## Concurrency evidence (successful jobs, one attempt)', '',
                  f'- Jobs with comparable timestamps: {len(good)}.',
                  f'- Same-target overlapping pairs: {len(overlaps["same"])}.',
                  f'- Different-target overlapping pairs: {len(overlaps["different"])}.',
                  f'- Example different-target pairs: {overlaps["different"][:5]}.',
                  '', '## Final audit', '', '```text', *audit, '```', '',
                  '## Method and limitations', '',
                  '- HTTP metrics include authenticated API reads, login/logout, enqueue and polling within each measured phase; setup and final audit are excluded. Threshold: p95 < 1000 ms and HTTP error rate < 5%.',
                  '- Request tags are set at request start; job counts are attributed to the time their terminal status is observed. A job can finish in a later phase than its submission cohort. Throughput is count / phase duration, including ramp; drain has no fixed duration.',
                  '- Queue time = first start minus creation; duration = final finish minus first start (includes retry delays if any). Only jobs with valid start/finish timestamps contribute timing samples.',
                  '- Percentiles use linear interpolation on raw k6 samples; missing measurements are N/A. HTTP threshold table is calculated from raw samples; the k6 exit status is authoritative.',
                  '- Final audit is independent from load metrics. Non-success or unresolved jobs cannot be counted as successful migrations. Concurrency pairs describe worker job timestamp intervals, not precise SQL lock hold intervals.',
                  '- Fill in the hardware/software environment and interpretation of each level in the root README based on these actual results.', ''])
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text('\n'.join(lines), encoding='utf-8')
    print(f'Report saved: {args.report}')


if __name__ == '__main__':
    main()
