# Observability lokal

Stack opsional untuk development Milestone 2. Frontend, API, embedded worker dan PostgreSQL tetap berjalan di Windows. Docker Desktop hanya menjalankan Prometheus, OpenTelemetry Collector, Loki, Tempo dan Grafana. Konfigurasi ini bukan konfigurasi produksi. Lihat [definisi metrik](metrics.md) dan [pengujian k6](../../tests/k6/README.md).

## Aliran telemetry

- Prometheus mengambil `http://host.docker.internal:9464/metrics` setiap tiga detik.
- API dan embedded worker mengirim span OTLP/HTTP ke `http://127.0.0.1:4318/v1/traces`; Collector meneruskan ke Tempo melalui OTLP/gRPC.
- `dev.cmd` dalam mode observability mengarahkan stdout JSON backend ke `tmp/observability/logs/be.stdout.log`. Collector membaca file tersebut melalui receiver `filelog` dan mengirim ke Loki dengan OTLP/HTTP. Direktori kredensial `tmp/dev` tidak di-mount.
- Grafana memuat tiga datasource dan dashboard dari file provisioning. Log mempertahankan JSON asli dan tautan TraceID menuju Tempo.

## Prasyarat

Docker Desktop berjalan dengan Linux containers dan Docker Compose v2. Go, PostgreSQL, frontend dan `.env` lokal sudah disiapkan sesuai README utama. Gunakan embedded worker (`WORKER_EMBEDDED=true` atau default). Standalone worker tidak termasuk konfigurasi scrape/log ini.

## Menjalankan

Hentikan aplikasi lama dengan Ctrl+C. Semua perintah berikut dijalankan dari root repo, menggunakan PowerShell.

Terminal 1 (stack):

```powershell
docker version
docker compose version
New-Item -ItemType Directory -Force tmp/observability/logs | Out-Null
docker compose -f docker-compose.observability.yml config --quiet
docker compose -f docker-compose.observability.yml up -d
docker compose -f docker-compose.observability.yml ps
```

Opsional: set `$env:OBS_GRAFANA_PASSWORD` sebelum `up` untuk mengganti password awal Grafana (`admin` default hanya untuk lokal). Password pertama disimpan pada volume Grafana; mengubah variabel tidak mereset akun yang sudah ada. Jangan commit password. Jika sudah ada file log observability dari sesi lama, pindahkan ke folder lain saat backend dan Collector berhenti untuk menghindari replay.

Terminal 2 (aplikasi):

```powershell
$env:OBSERVABILITY_ENABLED = 'true'
.\dev.cmd
```

Flag ini berlaku per proses terminal, bukan perubahan `.env`. Mode ini mengatur listener metrics ke `0.0.0.0:9464` agar dapat di-scrape container, dan mengatur kedua variabel OTLP endpoint ke Collector lokal, menggantikan nilai sebelumnya untuk proses development ini. Backend harus dapat dijangkau oleh Docker Desktop; jangan membuka port 9464 ke internet atau jaringan publik. Batasi akses port dengan Windows Firewall; jangan menonaktifkan firewall. Mode default tanpa flag tetap loopback. Port Grafana, Prometheus dan OTLP yang dipublikasikan oleh Compose hanya di-bind pada loopback host. Loki dan Tempo tidak mempublikasikan port host.

| Layanan | URL |
|---|---|
| Aplikasi | http://localhost:3000 |
| Metrics | http://127.0.0.1:9464/metrics |
| Prometheus targets | http://localhost:9090/targets |
| Grafana | http://localhost:3001 |
| Dashboard | http://localhost:3001/d/terserah-observability |

## Validasi konfigurasi dan aliran data

```powershell
docker compose -f docker-compose.observability.yml run --rm --no-deps --entrypoint /bin/promtool prometheus check config /etc/prometheus/prometheus.yml
docker compose -f docker-compose.observability.yml run --rm --no-deps collector validate --config=/etc/otelcol-contrib/config.yaml
curl.exe -fsS http://127.0.0.1:9464/metrics
curl.exe -fsS 'http://127.0.0.1:9090/api/v1/query?query=up%7Bjob%3D%22terserah-api%22%7D'
```

Hasil `up` harus 1 setelah backend berjalan. Buka Grafana, login, lalu pilih Dashboards > Terserah > Terserah — Observability. Pastikan tiga datasource tersedia. Panel histogram berbasis `rate(...[1m])` dapat kosong/NaN sebelum ada observasi; counter batch dan row akan nol jika hanya menjalankan migration normal.

Untuk memverifikasi log dan trace, gunakan terminal k6 dengan environment yang telah disiapkan pada [README k6](../../tests/k6/README.md):

```powershell
k6 run --no-color --log-output=stdout tests/k6/smoke.js
```

Setelah job selesai dan data terkirim (export trace bersifat asynchronous), lakukan langkah berikut:

1. Di Grafana > Explore, pilih Loki, gunakan `{service_name="terserah-api"} | json | event="execution_finished"` pada rentang waktu pengujian.
2. Buka detail satu log, catat `job_id`, `execution_id`, `trace_id`, waktu dan status. Klik derived field TraceID untuk menuju Tempo; jika tautan tidak muncul, salin `trace_id` ke Explore > Tempo > Trace ID.
3. Cari span `migration.worker`, periksa `job.id` dan `execution.id`; cocokkan ID serta intervalnya dengan log. Trace parent enqueue dan worker mengikuti implementasi backend.
4. Buka dashboard pada rentang waktu yang sama. Jumlah job sukses seharusnya bertambah tiga apabila tidak ada job lain. Histogram queue/lock/duration menerima observasi baru, sedangkan pool dapat berubah selama aktivitas. Metrics tidak memiliki label job/trace ID; korelasi dengan metrics berdasarkan waktu dan aktivitas agregat, bukan relasi satu sampel per trace.

## Validasi workload k6 dengan observability aktif

Gunakan profil `quick` dari [pengujian k6](../../tests/k6/README.md) untuk mengamati perubahan queue, durasi executor dan pool ketika jumlah VU berubah. Jangan menimpa hasil stress test utama yang sudah disimpan.

```powershell
New-Item -ItemType Directory -Force tmp/observability | Out-Null
$env:K6_PROFILE = 'quick'
k6 run --no-color --log-output=stdout tests/k6/stress.js | Tee-Object tmp/observability/stress-summary.log
$LASTEXITCODE
```

Buka dashboard Grafana pada rentang yang mencakup pengujian dan ambil bukti grafik. Hasil k6 sebelumnya tetap terpisah karena overhead observability dapat memengaruhi performa.

## Validasi batch dan retry

Script `tests/k6/observability.js` menggunakan API dan workflow approval biasa untuk membuat empat migration baru pada database development `wbd_dev_target_a`. Gunakan konfigurasi environment pada README k6. Jangan jalankan pada database berisi data penting atau bersamaan dengan job lain. Backend tidak boleh di-restart di tengah pengujian karena counter/histogram bersifat process-local.

```powershell
$env:K6_CONFIRM_OBS_FAILURES = 'YES'
k6 run --no-color --log-output=stdout tests/k6/observability.js | Tee-Object tmp/observability/validation.log
$LASTEXITCODE
Remove-Item Env:\K6_CONFIRM_OBS_FAILURES -ErrorAction SilentlyContinue
```

| Kasus | Hasil yang diharapkan |
|---|---|
| Setup schema `obs_<run>` | Success; tabel 1.000 row dan fungsi fixture |
| Batch UPDATE | Success; 10 batch masing-masing 100 row |
| Permanent failure `SELECT 1/0` | Failed, SQLSTATE 22012; 1 attempt |
| Transient failure `retry_probe()` | Failed, SQLSTATE 40001; 3 attempt (2 retry otomatis) |

Kegagalan pada dua kasus terakhir sengaja dibuat dan tidak mengubah data yang sudah ada. Fungsi uji selalu melempar error untuk menguji batas retry, bukan simulasi bahwa layanan benar-benar terputus. Fixture dan history tetap tersimpan; batch ditandai irreversible hanya untuk pengujian ini. Script memeriksa status terminal dan attempt history melalui API, lalu membandingkan snapshot metrics lokal sebelum/sesudah pengujian.

Perubahan metrics yang diharapkan jika tidak ada job lain: success +2, failed +2, retry attempts +2, queue initial count +4, queue retry count +2, executor success count +2, executor failed count +4, batch migration counter +10 dan row migration counter +1.000. Hasil `OBS_METRIC` dan `OBS_RESULT` di log menunjukkan hasil aktual. Exit code 0 berarti semua checks dan threshold script terpenuhi; dua job failed tetap merupakan outcome yang diharapkan. Jika hasil berbeda, simpan log dan diagnosis; jangan menghapus history atau mengulang pengujian sebelum memeriksa job yang belum terminal.

Bukti awal tersedia pada [direktori evidence](../../observability/evidence/).

## Troubleshooting

- `docker compose ... ps -a` dan `docker compose ... logs --tail=80 collector loki tempo prometheus grafana` menunjukkan service yang gagal. Gunakan nama file Compose pada setiap perintah.
- Prometheus DOWN: pastikan `curl.exe` ke metrics lokal sukses, flag observability aktif, Docker Desktop dapat mengakses `host.docker.internal:9464`, dan firewall mengizinkan jalur lokal tersebut. Jangan menonaktifkan firewall.
- Log kosong: pastikan `tmp/observability/logs/be.stdout.log` bertambah. Collector membaca hanya stdout JSON backend; stderr diagnostik tetap lokal. Filelog menggunakan offset dalam memori; restart Collector bisa membaca ulang log. Bila backend me-restart dan file terpotong, periksa log terbaru; jika perlu hentikan backend dan Collector, arsipkan file lama, mulai Collector lalu backend kembali.
- Trace kosong: periksa log Collector dan `trace_export_failed` di stdout backend; pastikan aplikasi dijalankan dengan flag yang benar dan rentang waktu Grafana mencakup pengujian.
- Semua data lokal tanpa autentikasi pada Loki/Tempo di jaringan Compose; jangan gunakan stack untuk production. Versi image dipin agar reproduktif; Loki/Tempo berjalan sebagai root di container lokal agar named volume dapat ditulis (tidak ada writable bind mount ke host). Upgrade dan hardening diperlukan untuk deployment publik.

## Menghentikan

Ctrl+C pada terminal aplikasi. Jika ingin kembali ke mode biasa, jalankan:

```powershell
Remove-Item Env:\OBSERVABILITY_ENABLED -ErrorAction SilentlyContinue
.\dev.cmd
```

Hentikan stack dengan `docker compose -f docker-compose.observability.yml down`. Named volume mempertahankan data Prometheus, Loki, Tempo dan Grafana. Jangan gunakan `down -v` kecuali memang ingin menghapus data observability. Database PostgreSQL aplikasi tidak dikelola Compose ini.

## Referensi konfigurasi

- [OpenTelemetry filelog receiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/v0.122.0/receiver/filelogreceiver)
- [Loki OTLP ingestion](https://grafana.com/docs/loki/latest/send-data/otel/)
- [Tempo OTLP collector](https://grafana.com/docs/tempo/latest/set-up-for-tracing/instrument-send/set-up-collector/otel-collector/)
- [Grafana Loki derived fields](https://grafana.com/docs/grafana/latest/datasources/loki/configure/)
