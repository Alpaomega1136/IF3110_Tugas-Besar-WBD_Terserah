# Stress Testing dengan k6

Jalankan hanya pada instance lokal dan dua database disposable. Gunakan akun requester dan leader berbeda; seluruh group yang mempunyai grant ke target harus dapat disetujui oleh leader pengujian. Setup memakai API aplikasi untuk membuat draft, submit review, dan approval; tidak memodifikasi metadata langsung.

## Environment (PowerShell)

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

Jalankan aplikasi dan worker (`.\dev.cmd`). Smoke test: `k6 run tests/k6/smoke.js`. Tidak perlu reset database sebelum stress test, tetapi pastikan tidak ada job lain yang sedang berjalan. Setiap run membuat schema baru yang unik; fixture tetap tersimpan pada target.

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

## Workload dan pengukuran

| Level full | Ramp + hold | API VU target | Job A | Job B |
|---|---:|---:|---:|---:|
| Normal | 5 + 40 s | 10 | 2 | 2 |
| Medium | 5 + 55 s | 25 | 5 | 3 |
| Stress | 5 + 70 s | 50 | 12 | 4 |
| Recovery | 5 + 40 s | 10 | 2 | 2 |

API VU melakukan read database list, schema, migration history, dan job list, dengan jeda 0,5 s per iterasi; login satu kali per VU dan sesi read berakhir sesuai timeout aplikasi. Setiap level memiliki skenario submission terpisah yang mulai setelah ramp, satu approved review unik per VU (satu kali enqueue dan polling sampai terminal, maksimal 120 s, disusul audit). SQL fixture sama dengan smoke test: schema unik, tabel 1.000 row, dan `pg_sleep(0.5)` untuk memberi durasi terkontrol. Jumlah VU submission adalah tambahan terhadap VU API, bukan jumlah VU total. `quick` menggunakan VU 2/4/6/2 dengan total 15 job; `full` total 32 job. Setelah recovery, read VU turun ke nol dalam 5 s; job yang belum selesai tetap dipantau dalam batas waktu. Final audit memeriksa setiap review melalui daftar job dan memantau job yang masih berjalan dalam batas tambahan 90 s.

Threshold HTTP per level: p95 < 1.000 ms dan error rate < 5%. Threshold migration: seluruh planned job harus mencapai `success` pada workload dan final audit. Setup/audit tidak masuk metrik fase beban. Laporan generator memisahkan job accepted, terminal, success, retry, timing, dan throughput; juga memeriksa overlap waktu job berdasarkan timestamp backend. HTTP 202 hanya diterima antrean, bukan keberhasilan eksekusi. Jika job tidak selesai, catat status unresolved/timeout dan hasil audit, jangan klaim sukses.

Hasil pengujian dan analisis aktual dimasukkan ke README root setelah pengujian. Belum ada angka stress test yang diasumsikan.
