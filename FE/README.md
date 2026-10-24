
# Terserah — Frontend

Frontend IF3110 Milestone 1 untuk Database Management Platform.

## Teknologi

- Go 1.26.2 (`net/http` dan `html/template`)
- JavaScript ES Modules tanpa bundler
- HTML dan CSS
- Same-origin reverse proxy untuk backend Go

## Menjalankan

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
- `/admin/databases` — kelola koneksi PostgreSQL target (Platform Admin): daftar, tambah, edit, test, hapus
- `/databases/{id}/drafts` — editor migration draft, memerlukan akses `edit`
- `/databases/{id}/history` — riwayat eksekusi migration, memerlukan akses `view` atau `edit`

## Pengujian

Dari direktori `FE/`:

```powershell
go test ./...
go vet ./...
```
## Pengujian connection management

```powershell
# Dari FE/
node --experimental-default-type=module --test test/*.test.mjs
go test ./...
go vet ./...
```

Lakukan smoke test dengan BE gabungan dan PostgreSQL target terpisah dari metadata.
`APP_ORIGIN` BE harus sesuai origin browser FE (`http://localhost:3000`).
Kredensial target hanya dimasukkan saat pendaftaran/pergantian password, tidak dibaca ulang melalui API.
Jika test koneksi mengembalikan `metadata_target_forbidden`, hapus entri tersebut: metadata platform tidak boleh menjadi database target.
