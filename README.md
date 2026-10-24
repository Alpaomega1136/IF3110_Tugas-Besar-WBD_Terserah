# Terserah - Database Management Platform

Repository Milestone 1 IF3110 Pengembangan Aplikasi Web. Backend menggunakan Go dan target PostgreSQL; frontend disiapkan terpisah di `FE/`.

```text
FE/                          Project frontend
BE/
  cmd/api/                   Entry point HTTP server
  cmd/migrate/               CLI migration metadata platform
  cmd/seed/                  CLI bootstrap akun Platform Admin pertama
  internal/config/           Konfigurasi environment
  internal/database/         Pool PostgreSQL metadata
  internal/httpapi/          Router HTTP dan wiring endpoint
  internal/policy/           Policy akses database + unit test
  internal/auth/             Autentikasi, password hashing, session, rotasi/grace, CSRF
  internal/admin/            Manajemen user, group, membership, grant, dan daftar database
  internal/connections/      Tempat implementasi koneksi target (BE 2)
  internal/viewer/           Tempat implementasi schema/data viewer (BE 2)
  internal/migration/        Draft & history API (BE 1), runner target (BE 2)
  migrations/                SQL schema metadata + runner ter-embed
  tests/integration/         Integration test dengan PostgreSQL nyata
  tests/fixtures/            SQL/data pengujian dan demo
```

## Status

Sudah tersedia:
- Fondasi server HTTP dengan graceful shutdown, konfigurasi environment, pool metadata PostgreSQL, health endpoints, dan migration metadata idempotent.
- **Seluruh fitur BE 1**:
  - Bootstrap Platform Admin (`cmd/seed`) dan password hashing `PBKDF2-HMAC-SHA256`.
  - Login, logout, informasi pengguna aktif (`/api/v1/auth/me`), dan token CSRF (`/api/v1/auth/csrf`).
  - Opaque session cookie (`terserah_session`) dengan `HttpOnly`, `SameSite=Lax`, `Secure` (HTTPS/prod), idle timeout (30m), absolute expiration (8h), rotasi berkala (15m) & saat peningkatan privilege dengan grace period (30s), penanganan request bersamaan, serta revokasi session.
  - Manajemen user, group, membership multi-group, grant akses database (`view` / `edit`), dan endpoint daftar database terotorisasi (`GET /api/v1/databases`).
  - Pembuatan/pembaruan draft SQL migration dan pembacaan riwayat/detail migration (`BE/internal/migration`).
- Unit test dan PostgreSQL integration test untuk seluruh modul BE 1.

Masih perlu dikerjakan oleh anggota tim lain:
- **BE 2**: Pengelolaan koneksi target PostgreSQL & enkripsi kredensial (`internal/connections`), database schema/data viewer (`internal/viewer`), serta runner eksekusi transaksi SQL migration (`POST /api/v1/databases/{id}/drafts/{draftID}/execute`).
- **FE**: Antarmuka pengguna (`FE/`).

## Menjalankan Backend & Bootstrap Admin Secara Lokal

Prasyarat: Go 1.26+ dan PostgreSQL lokal yang sudah berjalan.

Buat user dan database metadata sekali menggunakan akun administrator PostgreSQL lokal (contoh: `postgres`):

```powershell
psql -h localhost -U postgres -d postgres -c "CREATE ROLE platform LOGIN PASSWORD 'platform_dev_only';"
createdb -h localhost -U postgres -O platform platform
```

Jalankan migration metadata, bootstrap akun Platform Admin pertama, lalu nyalakan server API dari root repository:

```powershell
Copy-Item .env.example .env
Set-Location BE
$env:DATABASE_URL = 'postgres://platform:platform_dev_only@localhost:5432/platform?sslmode=disable'
$env:HTTP_ADDR = ':8080'
$env:ADMIN_USERNAME = 'admin'
$env:ADMIN_PASSWORD = 'change_me_min_8_chars'
go mod download
go run ./cmd/migrate
go run ./cmd/seed
go run ./cmd/api
```

## Database Target Demo (Opsional)

```powershell
createdb -h localhost -U postgres -O platform demo_target
psql -h localhost -U platform -d demo_target -v ON_ERROR_STOP=1 -f BE/tests/fixtures/target.sql
```

## Pengujian

Dari `BE/`:
```powershell
go test ./...
go vet ./...
go build ./...
```
