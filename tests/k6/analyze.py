"""Menghasilkan laporan Markdown dari data JSON dan log peristiwa k6."""
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


def phase_label(name):
    return {
        'normal': 'Normal',
        'medium': 'Menengah',
        'stress': 'Stress',
        'recovery': 'Pemulihan',
        'drain': 'Penyelesaian antrean',
    }.get(name, name)


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
        raise SystemExit('Peristiwa K6_PROFILE tidak ditemukan: periksa setup dan format log mentah')
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
    lines = [f'# Pengujian beban dengan k6 — {profile["profile"]}', '', f'ID pengujian: `{profile["runId"]}`. Jumlah job yang direncanakan: {profile["totalJobs"]}.', '',
             '## Beban HTTP', '', '| Tingkat beban | Target VU API | Durasi (detik) | Jumlah request | Request/detik | p50 (ms) | p95 (ms) | Kesalahan HTTP (%) | Ambang batas |',
             '|---|---:|---:|---:|---:|---:|---:|---:|---|']
    for p in profile['phases']:
        m = by_phase[p['name']]
        req = int(sum(m['http_reqs']))
        errors = m['http_req_failed']
        rate = sum(errors) / len(errors) * 100 if errors else None
        p50, p95 = (percentile(m['http_req_duration'], q) for q in (.5, .95))
        threshold = 'N/A' if p95 is None or rate is None else ('LULUS' if p95 < 1000 and rate < 5 else 'TIDAK LULUS')
        lines.append(f'| {phase_label(p["name"])} | {p["vus"]} | {p["duration"]} | {req} | {fmt(req/p["duration"])} | {fmt(p50)} | {fmt(p95)} | {fmt(rate)} | {threshold} |')
    lines.extend(['', '## Throughput migration berdasarkan fase pengamatan', '',
                  '| Tingkat beban | Dicoba | Diterima | Mencapai status terminal | Berhasil | Tidak berhasil | Belum terkonfirmasi | Terminal/detik | Berhasil/detik | Waktu antre p50/p95 (ms) | Durasi p50/p95 (ms) |',
                  '|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|'])
    for p in [*profile['phases'], {'name': 'drain', 'duration': None}]:
        m = by_phase[p['name']]
        a, b, c, d, e, u = (int(sum(m['stress_jobs_'+key])) for key in ('attempted','accepted','terminal','success','unsuccessful','unresolved'))
        sec = p['duration']
        lines.append(f'| {phase_label(p["name"])} | {a} | {b} | {c} | {d} | {e} | {u} | {fmt(c/sec if sec else None)} | {fmt(d/sec if sec else None)} | {timing(m["stress_queue_wait_ms"])} | {timing(m["stress_job_duration_ms"])} |')
    lines.extend(['', '## Job berdasarkan fase pengiriman', '', '| Tingkat beban | Rencana A/B | Diterima | Terminal | Berhasil | Percobaan ulang | Waktu antre p50/p95 (ms) | Durasi p50/p95 (ms) |', '|---|---|---:|---:|---:|---:|---|---|'])
    for p in profile['phases']:
        m = by_cohort[p['name']]
        a,b,c,r = (int(sum(m['stress_'+k])) for k in ('jobs_accepted','jobs_terminal','jobs_success','job_retries'))
        lines.append(f'| {phase_label(p["name"])} | {p["a"]}/{p["b"]} | {a} | {b} | {c} | {r} | {timing(m["stress_queue_wait_ms"])} | {timing(m["stress_job_duration_ms"])} |')
    # Gunakan timestamp backend, bukan timestamp polling client, untuk memeriksa overlap.
    good = [j for j in jobs if j['status'] == 'success' and j['started_at'] and j['finished_at'] and j['attempts'] == 1]
    overlaps = defaultdict(list)
    for a,b in combinations(good,2):
        if max(stamp(a['started_at']), stamp(b['started_at'])) < min(stamp(a['finished_at']), stamp(b['finished_at'])):
            overlaps['same' if a['target'] == b['target'] else 'different'].append((a['job'], b['job']))
    lines.extend(['', '## Bukti konkurensi (job berhasil dalam satu percobaan)', '',
                  f'- Job dengan timestamp yang dapat dibandingkan: {len(good)}.',
                  f'- Pasangan job yang bertumpang tindih pada target yang sama: {len(overlaps["same"])}.',
                  f'- Pasangan job yang bertumpang tindih pada target berbeda: {len(overlaps["different"])}.',
                  f'- Contoh pasangan ID job pada target berbeda: {overlaps["different"][:5]}.',
                  '', '## Audit akhir', '', '```text', *audit, '```', '',
                  '## Metodologi dan batasan', '',
                  '- Metrik HTTP mencakup pembacaan API terautentikasi, login/logout, pengiriman job ke antrean, dan polling pada setiap fase pengukuran. Setup dan audit akhir tidak disertakan. Ambang batas: p95 < 1000 ms dan tingkat kesalahan HTTP < 5%.',
                  '- Tag request ditetapkan saat request dimulai. Jumlah job dihitung berdasarkan fase ketika status terminalnya teramati. Job dapat selesai pada fase yang berbeda dari fase pengirimannya. Throughput dihitung dengan membagi jumlah request atau job dengan durasi fase, termasuk perubahan beban (ramp). Fase penyelesaian antrean (drain) tidak memiliki durasi tetap.',
                  '- Waktu antre dihitung dari waktu pembuatan job hingga pertama kali dimulai. Durasi dihitung dari waktu mulai pertama hingga selesai (termasuk jeda percobaan ulang jika ada). Hanya job dengan timestamp mulai dan selesai yang valid yang digunakan dalam pengukuran waktu.',
                  '- Persentil dihitung menggunakan interpolasi linear pada sampel mentah k6. N/A menunjukkan bahwa data pengukuran tidak tersedia atau metrik tidak berlaku. Tabel ambang batas HTTP dihitung dari sampel mentah; status keluar k6 menjadi acuan utama.',
                  '- Audit akhir dilakukan secara terpisah dari metrik beban. Job yang tidak berhasil atau belum terkonfirmasi tidak dihitung sebagai migration berhasil. Pasangan konkurensi menggambarkan interval timestamp job pada worker, bukan interval kepemilikan SQL lock secara langsung.',
                  '- Lingkungan pengujian dan analisis hasil setiap tingkat beban didokumentasikan pada bagian Stress Testing dengan k6 di README utama repositori.', ''])
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text('\n'.join(lines), encoding='utf-8')
    print(f'Laporan disimpan: {args.report}')


if __name__ == '__main__':
    main()
