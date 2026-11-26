# Security: SAST dengan CodeQL

## Konfigurasi dan Hasil Scan

Static Application Security Testing (SAST) dilakukan menggunakan CodeQL melalui GitHub Actions dengan workflow [`.github/workflows/codeql.yml`](../../.github/workflows/codeql.yml). Workflow menggunakan Advanced Setup dengan default query suite, manual build untuk Go pada kedua modul (`BE/` dan `FE/`), dan `build-mode: none` untuk JavaScript/TypeScript. Analisis dijalankan tanpa menyalakan aplikasi atau database.

| Informasi | Nilai |
|---|---|
| Tanggal scan | 26 November 2026 |
| Branch | `feat/bonus-codeql` |
| Commit yang dianalisis | `5f44c76` |
| Workflow run | CodeQL #1 |
| Durasi workflow | 1 menit 34 detik |
| Versi CodeQL | 2.27.6 |
| Versi SARIF | 2.1.0 |

Branch, commit, nomor run, dan durasi diperoleh dari tampilan GitHub Actions. Versi CodeQL, hasil analisis, dan cakupan ekstraksi diperoleh dari kedua artifact SARIF.

| Analisis | Status eksekusi | Baris kode terukur (SARIF) | Temuan |
|---|---|---:|---:|
| Go | Berhasil | 9.811 | 0 |
| JavaScript/TypeScript | Berhasil | 6.983 | 0 |

Pada kedua file SARIF, `executionSuccessful` bernilai `true` dan `results` berupa array kosong. Tidak terdapat notifikasi berlevel `warning` atau `error` pada hasil tersebut. Halaman Code scanning untuk branch `feat/bonus-codeql` menunjukkan 0 open alerts dan 0 closed alerts.

## Cakupan Analisis

| Komponen | File berhasil diekstraksi |
|---|---:|
| Go backend (`BE/`) | 61 |
| Go frontend (`FE/`) | 4 |
| Go module manifest | 2 |
| JavaScript aplikasi (`.js`) | 22 |
| JavaScript test dan fixture (`.mjs`) | 15 |
| HTML template | 12 |
| GitHub Actions YAML (ekstraksi JavaScript) | 3 |

Cakupan diperoleh dari notifikasi `successfully-extracted-files` pada SARIF. Seluruh 65 file Go non-test dan 37 file JavaScript (`.js` dan `.mjs`) pada snapshot repository yang diperiksa berhasil diekstraksi. Kedua analisis mencatat 12 HTML template yang sama, sehingga tidak dihitung dua kali pada tabel. Ekstraksi file tidak berarti setiap file atau setiap baris diperiksa oleh semua query keamanan.

Query yang tercantum pada metadata SARIF antara lain `go/sql-injection`, `go/command-injection`, `go/clear-text-logging`, `go/cookie-httponly-not-set`, `go/cookie-secure-not-set`, `js/xss`, `js/sql-injection`, `js/path-injection`, dan `js/clear-text-logging`.

## Temuan dan Tindak Lanjut

Tidak terdapat temuan pada initial scan, sehingga tidak ada rule, severity, lokasi, atau dampak temuan yang perlu didokumentasikan per kasus. Tidak ada temuan yang diperbaiki, di-dismiss sebagai false positive, maupun diterima sebagai risiko. Commit perbaikan dan scan ulang terkait perbaikan temuan tidak berlaku pada hasil scan ini.

## Bukti Hasil Scan

- [Screenshot workflow berhasil](evidence/workflow-success.png)
- [Screenshot Code scanning alerts](evidence/code-scanning-alerts.png)
- [Artifact SARIF Go](evidence/codeql-go-sarif.zip)
- [Artifact SARIF JavaScript/TypeScript](evidence/codeql-javascript-typescript-sarif.zip)

![CodeQL workflow berhasil](evidence/workflow-success.png)

![Code scanning alerts pada branch feat/bonus-codeql](evidence/code-scanning-alerts.png)

## Reproduksi

Jalankan workflow CodeQL melalui push pada branch yang dikonfigurasi dalam workflow, pull request menuju branch yang dikonfigurasi, atau `workflow_dispatch` setelah workflow tersedia pada default branch. Periksa kedua job analisis pada tab Actions, unduh artifact SARIF dari halaman Summary, dan periksa Code scanning dengan filter branch yang sesuai.

## Batasan

Hasil scan bersih tidak berarti aplikasi pasti aman. Analisis terbatas pada commit yang disebutkan, file yang berhasil diekstraksi, dan query yang digunakan. Manual build menggunakan `go build ./...` sehingga 71 file Go `_test.go` pada snapshot repository tidak ikut diekstraksi. Analisis ini bukan DAST, audit manual menyeluruh, maupun pemeriksaan lengkap terhadap konfigurasi deployment dan seluruh dependency. Perubahan source code berikutnya perlu dianalisis kembali.
