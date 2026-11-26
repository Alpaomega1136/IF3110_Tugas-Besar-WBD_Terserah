# Observability lokal

Observability menyediakan pengumpulan telemetry terpusat untuk memantau aktivitas API, migration worker, serta connection pool PostgreSQL. Stack ini bersifat opsional pada lingkungan development Milestone 2. Frontend, API, embedded worker, dan PostgreSQL berjalan secara native di Windows; Docker Compose hanya mengelola layanan observability.

## Arsitektur

| Komponen | Peran | Konfigurasi |
|---|---|---|
| Prometheus | Scraping dan penyimpanan metrik | [prometheus.yml](../../observability/prometheus.yml) |
| OpenTelemetry Collector | Penerimaan, pemrosesan, dan penerusan log serta trace | [otel-collector.yml](../../observability/otel-collector.yml) |
| Loki | Penyimpanan dan pencarian log JSON | [loki.yml](../../observability/loki.yml) |
| Tempo | Penyimpanan dan penelusuran distributed trace | [tempo.yml](../../observability/tempo.yml) |
| Grafana | Visualisasi metrik, pencarian log dan trace | [Provisioning](../../observability/grafana/provisioning/), [dashboard JSON](../../observability/grafana/dashboards/terserah.json) |

Prometheus melakukan scraping terhadap `http://host.docker.internal:9464/metrics` setiap tiga detik. API dan embedded worker mengirim span melalui OTLP/HTTP ke `http://127.0.0.1:4318/v1/traces`. Collector meneruskan span ke Tempo melalui OTLP/gRPC.

Mode observability pada `dev.cmd` mengarahkan stdout JSON backend ke `tmp/observability/logs/be.stdout.log`. Receiver `filelog` membaca file tersebut, mempertahankan isi JSON, dan meneruskan log ke Loki melalui OTLP/HTTP. Direktori `tmp/dev` yang menyimpan kredensial tidak di-mount ke container. Grafana memuat tiga datasource dan dashboard berisi 17 panel melalui provisioning. Derived field `TraceID` menghubungkan log Loki dengan trace di Tempo.

Definisi, satuan, jenis, label, dan batasan metrik didokumentasikan pada [Metrik Observability](metrics.md).

## Konfigurasi dan reproduksi lingkungan

Lingkungan lokal memerlukan Docker Desktop dengan Linux containers dan Docker Compose v2, serta instalasi Go dan PostgreSQL sesuai [README utama](../../README.md). Konfigurasi ini menggunakan embedded worker (`WORKER_EMBEDDED=true` atau nilai bawaan). Standalone worker tidak termasuk target scrape dan pengumpulan log stack ini.

Seluruh perintah berikut menggunakan PowerShell dari root repository. Stack observability dan aplikasi dijalankan dalam dua proses terminal terpisah. Apabila instance aplikasi sebelumnya masih aktif, instance tersebut perlu dihentikan sebelum mode observability dijalankan.

**Inisialisasi stack:**

```powershell
docker version
docker compose version
New-Item -ItemType Directory -Force tmp/observability/logs | Out-Null
docker compose -f docker-compose.observability.yml config --quiet
docker compose -f docker-compose.observability.yml up -d
docker compose -f docker-compose.observability.yml ps
```

**Inisialisasi aplikasi:**

```powershell
$env:OBSERVABILITY_ENABLED = 'true'
.\dev.cmd
```

Flag `OBSERVABILITY_ENABLED` berlaku pada sesi terminal dan tidak mengubah `.env`. Mode ini menetapkan `METRICS_ADDR=0.0.0.0:9464`, `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`, dan `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:4318/v1/traces`. Listener metrik dapat dijangkau dari Docker Desktop, sedangkan mode development biasa tetap menggunakan konfigurasi loopback bawaan.

| Layanan | Alamat lokal |
|---|---|
| Aplikasi | http://localhost:3000 |
| Metrics | http://127.0.0.1:9464/metrics |
| Prometheus targets | http://localhost:9090/targets |
| Grafana | http://localhost:3001 |
| Dashboard | http://localhost:3001/d/terserah-observability |

Grafana menggunakan akun awal `admin` dengan password lokal bawaan `admin`. Variabel `OBS_GRAFANA_PASSWORD` dapat menetapkan password awal sebelum stack pertama kali dijalankan. Kredensial Grafana tersimpan pada named volume sehingga perubahan variabel tersebut tidak mereset akun yang sudah ada. Kredensial tidak disimpan dalam repository.

## Metodologi validasi

### Integrasi metrics, logs, dan traces

Validasi konfigurasi dan konektivitas layanan menggunakan perintah berikut:

```powershell
docker compose -f docker-compose.observability.yml run --rm --no-deps --entrypoint /bin/promtool prometheus check config /etc/prometheus/prometheus.yml
docker compose -f docker-compose.observability.yml run --rm --no-deps collector validate --config=/etc/otelcol-contrib/config.yaml
curl.exe -fsS http://127.0.0.1:9464/metrics
curl.exe -fsS 'http://127.0.0.1:9090/api/v1/query?query=up%7Bjob%3D%22terserah-api%22%7D'
```

Target Prometheus yang dapat di-scrape memiliki nilai `up=1`. Smoke test menghasilkan tiga migration melalui workflow approval dan enqueue; environment pengujian k6 mengikuti [dokumentasi k6](../../tests/k6/README.md).

```powershell
k6 run --no-color --log-output=stdout tests/k6/smoke.js
```

Log penyelesaian migration dapat ditemukan melalui Grafana Explore dengan datasource Loki menggunakan LogQL:

```logql
{service_name="terserah-api"} |= "execution_finished"
```

Korelasi log dan trace menggunakan `trace_id`, dengan `job_id` dan `execution_id` sebagai konteks eksekusi. Derived field TraceID membuka trace di Tempo; pencarian manual berdasarkan Trace ID juga tersedia. Span `migration.worker`, `migration.lock`, dan `db.query` menunjukkan aktivitas worker dan operasi database. Metrik pada dashboard dikorelasikan dengan rentang waktu pengujian dan perubahan agregat, bukan dengan label berisi ID individual.

### Workload k6 dengan observability aktif

Profil `quick` pada `stress.js` digunakan untuk mengamati perubahan antrean, durasi executor, dan penggunaan connection pool selama peningkatan dan penurunan beban. Pengujian ini terpisah dari pengukuran performa k6 utama karena instrumentasi dan pengumpulan telemetry dapat menambah overhead.

```powershell
New-Item -ItemType Directory -Force tmp/observability | Out-Null
$env:K6_PROFILE = 'quick'
k6 run --no-color --log-output=stdout tests/k6/stress.js | Tee-Object tmp/observability/stress-summary.log
$LASTEXITCODE
```

### Validasi batch dan retry

[observability.js](../../tests/k6/observability.js) menggunakan API dan workflow approval biasa untuk membuat empat migration pada database development `wbd_dev_target_a`. Script membandingkan status terminal dan attempt history melalui API serta snapshot metrics sebelum dan sesudah pengujian.

| Kasus | Kriteria keberhasilan validasi |
|---|---|
| Setup schema `obs_<run>` | Success; tabel 1.000 row dan fungsi fixture terbentuk |
| Batch UPDATE | Success; 10 batch masing-masing 100 row |
| Permanent failure `SELECT 1/0` | Failed, SQLSTATE `22012`, satu attempt |
| Retryable failure `retry_probe()` | Failed, SQLSTATE `40001`, tiga attempt (dua retry otomatis) |

Pengujian dibatasi pada database development yang dapat dibuang, tanpa job lain yang sedang diproses. Backend tetap berjalan selama pengujian agar counter dan histogram tidak mengalami reset. Fixture menggunakan schema unik; fixture dan history tetap tersimpan. Batch ditandai irreversible untuk pengujian ini. Fungsi `retry_probe()` selalu melempar error untuk menguji batas retry, bukan mensimulasikan gangguan koneksi layanan yang sebenarnya.

```powershell
$env:K6_CONFIRM_OBS_FAILURES = 'YES'
k6 run --no-color --log-output=stdout tests/k6/observability.js | Tee-Object tmp/observability/validation.log
$LASTEXITCODE
Remove-Item Env:\K6_CONFIRM_OBS_FAILURES -ErrorAction SilentlyContinue
```

Hasil `OBS_METRIC` mencatat perubahan tiap metrik, sedangkan `OBS_RESULT` merangkum validasi. Exit code `0` menunjukkan seluruh checks dan threshold terpenuhi. Status `failed` pada dua skenario kegagalan merupakan hasil yang memang diharapkan.

## Hasil pengujian

Smoke test dan quick stress test dengan observability aktif menghasilkan kenaikan jumlah job sukses dari 59 menjadi 74 pada pengujian workload. Validasi batch dan retry berikutnya (`mwxojez7_6e9361`) menghasilkan 106/106 checks lulus, 4/4 kasus terverifikasi, 49 HTTP request tanpa error, dan sembilan delta metrik sesuai ekspektasi. Jumlah job sukses bertambah menjadi 76, dengan dua job gagal terkontrol, dua retry otomatis, 10 batch committed, dan 1.000 row committed.

| Bukti | Cakupan |
|---|---|
| [Prometheus targets](../../observability/evidence/prometheus-targets.png) | Target scrape backend UP |
| [Log migration](../../observability/evidence/loki-migration-logs.png) | Structured log dan konteks trace |
| [Trace migration](../../observability/evidence/tempo-migration-trace.png) | Trace enqueue, worker, lock, dan query database |
| [Dashboard awal](../../observability/evidence/grafana-dashboard.png) | Metrik migration dan connection pool |
| [Dashboard stress test](../../observability/evidence/grafana-stress-test.png) | Aktivitas metrik selama workload |
| [Dashboard batch/retry](../../observability/evidence/grafana-batch-retry.png) | Status job, batch, row, dan retry |
| [Log batch](../../observability/evidence/loki-batch-logs.png) | Event `batch_committed` |
| [Log retry](../../observability/evidence/loki-retry-logs.png) | Event `job_recovered` |
| [Log validasi](../../observability/evidence/validation.log) | Hasil checks, attempt history, dan sembilan delta metrik |

Rincian angka serta contoh korelasi aktual tersedia pada [bagian Observability di README utama](../../README.md#observability). Pengujian 1.000 row memvalidasi instrumentasi batch; pengujian ini bukan pengganti uji dataset besar untuk fitur batch utama.

## Operasional dan batasan

| Kondisi | Penjelasan dan diagnosis |
|---|---|
| Container tidak berjalan | Status dan log layanan tersedia melalui `docker compose -f docker-compose.observability.yml ps -a` dan `docker compose -f docker-compose.observability.yml logs --tail=80`. |
| Target Prometheus DOWN | Konektivitas endpoint lokal, flag observability, akses Docker ke `host.docker.internal:9464`, dan aturan firewall merupakan titik pemeriksaan utama. |
| Log tidak tersedia | File `tmp/observability/logs/be.stdout.log` menjadi sumber log Collector; stderr diagnostik tetap lokal. |
| Trace tidak tersedia | Log Collector, event `trace_export_failed`, konfigurasi OTLP, dan rentang waktu Grafana dapat digunakan untuk diagnosis. |
| Panel histogram kosong | Histogram berbasis `rate(...[1m])` tidak menghasilkan percentile jika tidak terdapat observasi baru. Rate batch/row kembali nol setelah aktivitas selesai. |

Filelog menggunakan offset dalam memori sehingga restart Collector dapat menyebabkan pembacaan ulang log. Pengarsipan file lama ketika backend dan Collector berhenti dapat menghindari replay pada sesi baru. Port Grafana, Prometheus, dan OTLP yang dipublikasikan Compose hanya terikat pada loopback; Loki dan Tempo tidak mempublikasikan port host. Listener metrik port 9464 memerlukan pembatasan akses melalui firewall lokal. Stack tidak ditujukan untuk akses jaringan publik atau deployment production. Loki dan Tempo berjalan sebagai root di container lokal untuk akses tulis named volume; hardening diperlukan sebelum deployment publik.

## Siklus hidup stack

Setelah aplikasi dihentikan, mode development biasa dapat diaktifkan kembali melalui:

```powershell
Remove-Item Env:\OBSERVABILITY_ENABLED -ErrorAction SilentlyContinue
.\dev.cmd
```

Stack observability dihentikan dengan:

```powershell
docker compose -f docker-compose.observability.yml down
```

Named volume mempertahankan data Prometheus, Loki, Tempo, dan Grafana. Opsi `down -v` menghapus volume tersebut. PostgreSQL aplikasi tidak dikelola oleh Compose ini.

## Referensi

- [OpenTelemetry filelog receiver](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/v0.122.0/receiver/filelogreceiver)
- [Loki OTLP ingestion](https://grafana.com/docs/loki/latest/send-data/otel/)
- [Tempo OTLP collector](https://grafana.com/docs/tempo/latest/set-up-for-tracing/instrument-send/set-up-collector/otel-collector/)
- [Grafana Loki derived fields](https://grafana.com/docs/grafana/latest/datasources/loki/configure/)
