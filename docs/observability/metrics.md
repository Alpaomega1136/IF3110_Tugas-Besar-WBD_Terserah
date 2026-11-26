# Metrik Observability

Backend mengekspos Prometheus text exposition format 0.0.4 tanpa dependensi Go tambahan. Listener metrik terpisah dari API dan default hanya dapat diakses secara lokal:

- API (termasuk embedded worker): `http://127.0.0.1:9464/metrics`.
- Worker standalone: `http://127.0.0.1:9465/metrics` (metrik proses worker saja).

`METRICS_ADDR` dapat digunakan untuk mengganti alamat listener. Jika API dan worker dijalankan sebagai dua proses, gunakan port berbeda; konfigurasi default sudah berbeda. Jangan publikasikan listener metrik ke internet. Listener ini bukan endpoint untuk pengguna aplikasi dan tidak diteruskan melalui frontend.

## Definisi metrik

| Metrik | Tipe | Label | Definisi |
|---|---|---|---|
| `terserah_migration_jobs` | Gauge | `status` | Jumlah job pada setiap status saat scrape, termasuk job terminal dari sebelum restart. Hanya API yang mengekspor agregat metadata ini. |
| `terserah_migration_retry_attempts` | Gauge | - | Jumlah attempt tersimpan dengan `attempt_no > 1`. Tidak termasuk resume/compensation yang merupakan job baru. |
| `terserah_migration_queue_wait_seconds` | Histogram | `attempt` | Waktu sampai claim berhasil: sejak pembuatan job untuk attempt pertama atau `available_at` untuk retry. |
| `terserah_migration_lock_wait_seconds` | Histogram | `scope`, `result` | Waktu eksekusi perintah advisory lock untuk migration normal/batch, termasuk overhead SQL dan perintah yang gagal. Bukan waktu semua jenis lock PostgreSQL. |
| `terserah_migration_duration_seconds` | Histogram | `outcome` | Durasi satu pemanggilan executor worker (termasuk target pool dan lock, tanpa queue wait, finalisasi metadata atau recovery). Error tak terselesaikan dilabeli `unknown`. |
| `terserah_migration_batches_total` | Counter | `kind` | Batch migration/compensation baru yang commit-nya terkonfirmasi pada proses ini. |
| `terserah_migration_rows_total` | Counter | `kind` | Jumlah row pada batch yang commit-nya terkonfirmasi. Tidak menghitung DML normal. |
| `terserah_db_pool_connections` | Gauge | `role`, `state` | Koneksi acquired (active) dan idle dari pool hidup, dijumlahkan berdasarkan peran metadata/target. |
| `terserah_db_target_hijacked_connections` | Gauge | - | Koneksi guard target yang telah di-hijack untuk advisory lock dan berada di luar pool. |
| `terserah_db_pools` | Gauge | `role` | Jumlah pool hidup yang terdaftar. |
| `terserah_db_pool_acquires_total` | Counter | `role` | Jumlah acquire sukses, termasuk akumulasi dari pool yang sudah ditutup. |
| `terserah_db_pool_empty_acquires_total` | Counter | `role` | Jumlah acquire sukses yang harus menunggu pool kosong. |
| `terserah_db_pool_canceled_acquires_total` | Counter | `role` | Jumlah acquire yang dibatalkan. |
| `terserah_db_pool_acquire_seconds_total` | Counter | `role` | Akumulasi durasi acquire sukses, termasuk yang langsung tersedia. |
| `terserah_db_pool_wait_seconds_total` | Counter | `role` | Akumulasi waktu tunggu acquire sukses ketika pool kosong, termasuk waktu pembuatan koneksi. |

Histogram mengekspos seri `_bucket`, `_sum`, `_count`. Semua label menggunakan himpunan nilai terbatas; ID job, user, database, SQL, trace ID dan kredensial tidak menjadi label. ID korelasi tetap berada pada log dan trace yang sudah ada. Counter dan histogram bersifat process-local dan reset pada restart; status job dan retry dibaca dari metadata. Batch yang commit-nya ambigu lalu hanya dikonfirmasi melalui recovery tidak menambah counter commit teramati. Koneksi hijacked dilaporkan terpisah, sehingga tidak disalahartikan sebagai pool aktif. Statistik pool pendek yang sudah ditutup tetap diakumulasikan sampai restart.

Endpoint melakukan agregasi jumlah job per status dan jumlah retry dengan batas waktu dua detik. Jika metadata tidak tersedia, scrape API menghasilkan HTTP 503, bukan angka nol yang menyesatkan. Query scrape juga menggunakan pool metadata sehingga dapat sedikit memengaruhi statistik acquire. Pada volume metadata sangat besar, evaluasi biaya agregasi sebelum menentukan interval scrape. Gauge koneksi/pool merupakan snapshot; pool yang berumur sangat singkat bisa tidak terlihat pada satu scrape.

## Verifikasi lokal

Jalankan unit test backend tanpa tag integration:

```powershell
Push-Location BE
try { go test ./... } finally { Pop-Location }
```

Hentikan server lama dengan Ctrl+C, lalu jalankan `.\dev.cmd` dari root. Tidak perlu reset DB atau mengubah secret. Dari terminal lain:

```powershell
curl.exe -fsS http://127.0.0.1:9464/metrics
New-Item -ItemType Directory -Force tmp/observability | Out-Null
curl.exe -fsS http://127.0.0.1:9464/metrics -o tmp/observability/before.prom
```

Jalankan smoke test menggunakan environment k6 yang dijelaskan pada [README k6](../../tests/k6/README.md). Setelah tiga migration sukses:

```powershell
k6 run --no-color --log-output=stdout tests/k6/smoke.js
curl.exe -fsS http://127.0.0.1:9464/metrics -o tmp/observability/after.prom
Select-String -Path tmp/observability/before.prom,tmp/observability/after.prom -Pattern '^terserah_migration_jobs\{status="success"\}','^terserah_migration_queue_wait_seconds_count\{attempt="initial"\}','^terserah_migration_duration_seconds_count\{outcome="success"\}','^terserah_migration_lock_wait_seconds_count\{scope="session",result="success"\}','^terserah_db_pool_acquires_total\{role="target"\}','^terserah_db_pool_wait_seconds_total\{role="target"\}'
```

Jika tidak ada job lain dan server tidak direstart, status success, jumlah observasi queue initial, duration success, dan lock session success masing-masing bertambah tiga. Counter acquire target bertambah; waktu tunggu target tidak berkurang. Gauge queue/running dan pool target dapat nol setelah pekerjaan selesai. Smoke test menggunakan migration normal, sehingga counter batch/row dapat tetap nol; uji batch terpisah diperlukan untuk memvalidasi keduanya. Retry hanya bertambah jika ada attempt berikutnya pada job yang sama. Jangan sengaja merusak data untuk menguji failure/retry; gunakan fixture khusus di lingkungan development.

## Contoh PromQL untuk dashboard

- Status job: `terserah_migration_jobs` (scrape API saja, jangan menjumlahkan beberapa replika API).
- p95 antrean: `histogram_quantile(0.95, sum by (le) (rate(terserah_migration_queue_wait_seconds_bucket[5m])))`.
- p95 durasi executor: `histogram_quantile(0.95, sum by (le) (rate(terserah_migration_duration_seconds_bucket[5m])))`.
- Rata-rata wait ketika pool kosong: `sum by (role) (rate(terserah_db_pool_wait_seconds_total[5m])) / sum by (role) (rate(terserah_db_pool_empty_acquires_total[5m]))` (NaN jika tidak ada acquire yang menunggu).
- Pool active/idle: `sum by (role,state) (terserah_db_pool_connections)`.
- Batch/row per detik: `sum by (kind) (rate(terserah_migration_batches_total[5m]))` dan `sum by (kind) (rate(terserah_migration_rows_total[5m]))`.

Prometheus, OpenTelemetry Collector, centralized log/trace backend dan provisioning Grafana dikonfigurasi secara terpisah dari instrumentasi ini.
