# Terserah - Database Management Platform

Fondasi repository Milestone 1 IF3110. Backend menggunakan Go dan target PostgreSQL; frontend disiapkan terpisah untuk pengembang FE.

```text
FE/                          Project frontend (belum dipilih framework)
BE/
  cmd/api/                   Entry point HTTP server
  cmd/migrate/               CLI migration metadata platform
  internal/config/           Konfigurasi environment
  internal/database/         Pool PostgreSQL metadata
  internal/httpapi/          Router dan health endpoints
  internal/policy/           Policy akses database + unit test
  internal/auth/             Tempat implementasi auth/session/CSRF
  internal/admin/            Tempat implementasi user/group/grant
  internal/connections/      Tempat implementasi koneksi target
  internal/viewer/           Tempat implementasi schema/data viewer
  internal/migration/        Tempat implementasi draft/history/runner target
  migrations/                SQL schema metadata + runner ter-embed
  tests/integration/         Test dengan PostgreSQL nyata
  tests/fixtures/            SQL/data pengujian dan demo
docs/                        Arsitektur, kontrak API, backlog, testing
refs/                        Spesifikasi asli dan referensi tim
tests/e2e/                   Tempat skenario lintas FE-BE
.github/workflows/           CI backend
```

## Status

Sudah tersedia: server dengan graceful shutdown, konfigurasi environment, pool metadata PostgreSQL, health endpoints, migration metadata idempotent, policy multi-group, unit/integration test fondasi, dan CI.

**Ini setup/fondasi, bukan seluruh implementasi fitur Milestone 1.** Auth/session/CSRF, API administrasi, pengelolaan target, viewer, draft/runner/history migration, bootstrap admin, dan seluruh UI masih perlu dikerjakan. Folder modul memiliki README tanggung jawab; endpoint fitur belum didaftarkan. Lihat [backlog](docs/backlog.md).

## Menjalankan backend secara lokal

Prasyarat: Go 1.26+ ([unduhan resmi](https://go.dev/dl/)) dan PostgreSQL 17 lokal yang sudah berjalan. Pastikan `go`, `psql`, dan `createdb` tersedia di PATH.

Buat user dan database metadata sekali, menggunakan akun administrator PostgreSQL lokal (contoh: `postgres`). Masukkan password administrator saat diminta:

```powershell
psql -h localhost -U postgres -d postgres -c "CREATE ROLE platform LOGIN PASSWORD 'platform_dev_only';"
createdb -h localhost -U postgres -O platform platform
```

Jalankan dari root repository:

```powershell
Copy-Item .env.example .env
Set-Location BE
$env:DATABASE_URL = 'postgres://platform:platform_dev_only@localhost:5432/platform?sslmode=disable'
$env:HTTP_ADDR = ':8080'
go mod download
go run ./cmd/migrate
go run ./cmd/api
```

Sesuaikan `DATABASE_URL` dengan user, password, port, dan nama database lokal. `.env.example` adalah acuan konfigurasi; salinan `.env` opsional sebagai catatan lokal. Aplikasi Go membaca environment proses, **tidak** otomatis memuat `.env`. Di shell POSIX gunakan `export DATABASE_URL='...'`. Kredensial contoh hanya untuk development; karakter khusus pada password di connection URL harus di-URL-encode.

Periksa dari terminal lain:

```powershell
Invoke-RestMethod http://localhost:8080/health/live
Invoke-RestMethod http://localhost:8080/health/ready
```

Di macOS/Linux gunakan `curl` untuk health check. Hentikan server dengan `Ctrl+C`. Migration metadata bersifat forward-only: tambahkan file bernomor berikutnya, jangan ubah file yang sudah diterapkan. CLI ini berbeda dari fitur migration SQL database target.

## Database target demo (opsional)

Buat database terpisah pada PostgreSQL lokal; jalankan perintah berikut dari root repository sekali untuk database demo baru:

```powershell
createdb -h localhost -U postgres -O platform demo_target
psql -h localhost -U platform -d demo_target -v ON_ERROR_STOP=1 -f BE/tests/fixtures/target.sql
```

Fixture membuat tabel dan data contoh. Target belum otomatis terdaftar di metadata karena API koneksi belum dibuat. Jangan jalankan fixture pada database metadata atau database berisi data pribadi.

## Pengujian dan panduan tim

Dari `BE/`: `go test ./...`, `go vet ./...`, dan `go build ./...`.
Integration test memerlukan database khusus: [panduan testing](docs/testing.md).

- [Arsitektur dan keputusan awal](docs/architecture.md)
- [API tersedia dan rencana kontrak FE-BE](docs/api.md)
- [Pembagian kerja dan backlog](docs/backlog.md)
- [Referensi tugas](refs/README.md)

Jangan commit `.env`, password, connection URL pribadi, session token, atau hasil dump berisi data sensitif. Tidak ada akun admin/default login pada setup ini; buat bootstrap admin dengan password hashing saat modul auth dikerjakan.

