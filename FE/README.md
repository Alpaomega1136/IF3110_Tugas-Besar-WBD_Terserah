
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
- `/admin/grants` — pilih koneksi PostgreSQL nyata, atur/revoke grant View atau Edit untuk group
- `/admin/databases` — kelola koneksi PostgreSQL target (Platform Admin): daftar, tambah, edit, test, hapus
- `/databases/{id}/viewer` — explorer PostgreSQL read-only: schemas, tables, columns, constraints, indexes, rows, pagination
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

## grant dan workspace

Admin mendaftarkan target pada `/admin/databases`, membuat group dan membership,
kemudian memilih target nyata pada `/admin/grants`. Dashboard menampilkan hanya
database dari `/api/v1/databases` (grant efektif). `view` membuka Viewer/History;
`edit` juga dapat membuka Draft. Platform Admin tidak otomatis mendapat grant target.
Setelah grant berubah, anggota yang terpengaruh mungkin perlu login ulang karena
session dicabut oleh backend. Semua aksi tetap diperiksa ulang oleh BE.

## Database Viewer 

Buka `/databases/{id}/viewer` dari Dashboard setelah admin memberikan grant `view` atau `edit` melalui group. Pilih schema lalu tabel. Viewer memuat metadata (kolom, constraint, index) dan data baris menggunakan API baca BE2, tidak menjalankan SQL dan tidak mengubah target database.

Data baris dibatasi 10–200 per halaman (default 50) dengan `limit`/`offset`; urutan `primary_key` atau `physical` ditampilkan dari response BE. Nilai `NULL` dibedakan dari string kosong. Nama schema/tabel di-encode per segmen URL; data tidak pernah dimasukkan melalui `innerHTML`.

Jika target offline (503), pilih tombol muat ulang setelah koneksi pulih. Jika respons baris melebihi 4 MiB (413), turunkan jumlah baris per halaman. Pada 403 akses dicabut, workspace disembunyikan; pada 401 browser diarahkan ke login. Izin ditentukan oleh BE, bukan sekadar link frontend. Schema sistem tidak ditawarkan.

Tes regresi JS dan Viewer dapat dijalankan tanpa dependency npm:

```powershell
# Dari FE/
node --experimental-default-type=module --test test/*.test.mjs
go test ./...
go vet ./...
```

Pengujian browser memerlukan PostgreSQL target terpisah dari metadata, kredensial target valid, serta grant yang sesuai. Tes otomatis mock HTTP bukan pengganti uji browser FE–BE–PostgreSQL.
