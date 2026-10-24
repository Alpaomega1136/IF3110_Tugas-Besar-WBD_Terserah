
# Terserah — Frontend

Frontend IF3110 Milestone 1 untuk Database Management Platform.

## Teknologi

- Go 1.26.2 (`net/http` dan `html/template`)
- JavaScript ES Modules tanpa bundler
- HTML dan CSS
- Same-origin reverse proxy untuk backend Go

## Menjalankan

Jalankan BE pada port 8080 terlebih dahulu.

Dari direktori `FE/`:

```powershell
go run .
```

Frontend: http://localhost:3000

Environment opsional:

- `FE_HTTP_ADDR` — default `:3000`
- `FE_BACKEND_URL` — default `http://localhost:8080`

## Halaman

- `/login` — login
- `/dashboard` — akun dan daftar database yang dapat diakses
- `/admin/users` — administrasi pengguna
- `/admin/groups` — group dan membership
- `/admin/grants` — database grants untuk group
- `/databases/{id}/drafts` — editor migration draft, memerlukan akses `edit`
- `/databases/{id}/history` — riwayat eksekusi migration, memerlukan akses `view` atau `edit`

## Pengujian

Dari direktori `FE/`:

```powershell
go test ./...
go vet ./...
```
