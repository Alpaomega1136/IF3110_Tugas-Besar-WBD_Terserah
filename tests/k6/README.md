# Stress Testing dengan k6

Direktori ini berisi script stress testing k6, generator laporan, dan hasil pengujian Database Management Platform pada Milestone 2. Analisis hasil pengujian tersedia pada [README utama](../../README.md#stress-testing-dengan-k6).

## Prasyarat

- Grafana k6 CLI dan Python 3.9 atau lebih baru.
- Frontend, backend, PostgreSQL, dan worker berjalan pada instance development lokal (`.\dev.cmd`).
- Dua database PostgreSQL khusus pengujian yang telah terdaftar pada platform: `wbd_dev_target_a` dan `wbd_dev_target_b`. Jangan gunakan database berisi data penting.
- Dua akun berbeda, `k6_requester` dan `k6_leader`, dalam group pengujian dengan grant `View + Edit` pada kedua target. Tetapkan `k6_leader` sebagai group leader. Jika terdapat group lain yang memiliki akses ke target, seluruh approval yang diperlukan harus dapat diberikan oleh leader pengujian.

Setup fixture menggunakan API aplikasi untuk membuat draft, mengajukan review, dan memberikan approval tanpa memodifikasi metadata secara langsung.

## Konfigurasi environment (PowerShell)

```powershell
$env:K6_BASE_URL = 'http://localhost:3000'
$env:K6_APP_ORIGIN = 'http://localhost:3000'
$env:K6_TARGET_A = 'wbd_dev_target_a'
$env:K6_TARGET_B = 'wbd_dev_target_b'
$env:K6_CONFIRM_TEST_DB = 'YES'
$env:K6_USERNAME = 'k6_requester'
$env:K6_LEADER_USERNAME = 'k6_leader'
$env:K6_PASSWORD = [System.Net.NetworkCredential]::new('', (Read-Host 'Password requester' -AsSecureString)).Password
$env:K6_LEADER_PASSWORD = [System.Net.NetworkCredential]::new('', (Read-Host 'Password leader' -AsSecureString)).Password
```

Nilai `K6_TARGET_A` dan `K6_TARGET_B` adalah nama database PostgreSQL, bukan nama tampilan koneksi. Environment variable diatur pada terminal yang digunakan untuk menjalankan k6.

## Smoke test

Jalankan dari root repositori:

```powershell
k6 run --no-color --log-output=stdout tests/k6/smoke.js
```

Smoke test memvalidasi authentication, CSRF, pembuatan tiga migration, approval, enqueue, dan keberhasilan eksekusi. Database tidak perlu di-reset sebelum stress test, tetapi pastikan tidak ada job lain yang sedang berjalan. Setiap pengujian membuat schema dan review baru yang unik; fixture tetap tersimpan pada target.

## Stress test

Gunakan `quick` untuk validasi script, kemudian `full` untuk pengukuran. Perintah di root repo (Python 3.9+ untuk generator laporan):

```powershell
New-Item -ItemType Directory -Force tmp/k6 | Out-Null
$env:K6_PROFILE = 'quick'
k6 run --no-color --log-format=raw --log-output=file=tmp/k6/quick-events.log --out json=tmp/k6/quick-points.json tests/k6/stress.js | Tee-Object tmp/k6/quick-summary.log
$LASTEXITCODE
python tests/k6/analyze.py tmp/k6/quick-points.json tmp/k6/quick-events.log tmp/k6/quick-report.md
```

```powershell
$env:K6_PROFILE = 'full'
k6 run --no-color --log-format=raw --log-output=file=tmp/k6/full-events.log --out json=tmp/k6/full-points.json tests/k6/stress.js | Tee-Object tmp/k6/full-summary.log
$LASTEXITCODE
python tests/k6/analyze.py tmp/k6/full-points.json tmp/k6/full-events.log tmp/k6/full-report.md
```

Gunakan nama file baru (atau hapus hanya output pengujian lama) jika mengulang run supaya event tidak tercampur. Jika `python` tidak tersedia, gunakan `py -3`. Jangan commit raw JSON atau credentials. Simpan laporan Markdown dan evidence yang relevan untuk dokumentasi. Jika threshold gagal, laporkan kegagalan dan analisis; jangan menyembunyikan hasil.

## Beban dan pengukuran

| Level full | Ramp + hold | API VU target | Job A | Job B |
|---|---:|---:|---:|---:|
| Normal | 5 + 40 s | 10 | 2 | 2 |
| Medium | 5 + 55 s | 25 | 5 | 3 |
| Stress | 5 + 70 s | 50 | 12 | 4 |
| Recovery | 5 + 40 s | 10 | 2 | 2 |

API VU melakukan read database list, schema, migration history, dan job list, dengan jeda 0,5 s per iterasi; login satu kali per VU dan sesi read berakhir sesuai timeout aplikasi. Setiap level memiliki skenario submission terpisah yang mulai setelah ramp, satu approved review unik per VU (satu kali enqueue dan polling sampai terminal, maksimal 120 s, disusul audit). SQL fixture sama dengan smoke test: schema unik, tabel 1.000 row, dan `pg_sleep(0.5)` untuk memberi durasi terkontrol. Jumlah VU submission adalah tambahan terhadap VU API, bukan jumlah VU total. `quick` menggunakan VU 2/4/6/2 dengan total 15 job; `full` total 32 job. Setelah recovery, read VU turun ke nol dalam 5 s; job yang belum selesai tetap dipantau dalam batas waktu. Final audit memeriksa setiap review melalui daftar job dan memantau job yang masih berjalan dalam batas tambahan 90 s.

Threshold HTTP per level: p95 < 1.000 ms dan error rate < 5%. Threshold migration: seluruh planned job harus mencapai `success` pada workload dan final audit. Setup/audit tidak masuk metrik fase beban. Laporan generator memisahkan job accepted, terminal, success, retry, timing, dan throughput; juga memeriksa overlap waktu job berdasarkan timestamp backend. HTTP 202 hanya diterima antrean, bukan keberhasilan eksekusi. Jika job tidak selesai, catat status unresolved/timeout dan hasil audit, jangan klaim sukses.

## Hasil pengujian

Hasil aktual yang tersimpan dalam repositori:

| Profil | Laporan | Migration berhasil |
|---|---|---:|
| `quick` | [Laporan quick test](results/quick-report.md) | 15/15 |
| `full` | [Laporan full stress test](results/full-report.md) | 32/32 |

Kedua pengujian menghasilkan exit code `0`. Rincian performa API, waktu antre, durasi migration, throughput, pemulihan sistem, serta batasan pengujian dibahas pada [README utama](../../README.md#stress-testing-dengan-k6). Laporan menyimpan log audit akhir dalam format asli untuk mempertahankan bukti status setiap job.
