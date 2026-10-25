
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
- `/databases/{id}/drafts` — editor draft dan Execute Migration berkonfirmasi, memerlukan akses `edit`
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

## Execute Migration 

### Schema diff

Pada halaman Draft, pilih dua schema yang dapat diakses di database target yang sama: schema yang akan diubah dan schema acuan. Klik **Bandingkan Schema** untuk melihat perbedaan dan kandidat SQL, lalu **Gunakan SQL sebagai Draft Baru** untuk mengedit dan menyimpannya. Perbandingan tidak menjalankan SQL. Memerlukan grant Edit pada database dan hak PostgreSQL untuk membaca kedua schema.

Generator hanya membuat `CREATE TABLE` untuk tabel biasa dengan tipe sederhana yang didukung dan `ADD COLUMN` untuk kolom nullable tanpa default. Penghapusan, perubahan kolom yang ada, constraint, index, tabel partisi, default, identity, generated column, dan tipe lain perlu ditinjau manual; peringatan ditampilkan pada hasil. Schema acuan tidak otomatis disalin secara lengkap. Maksimum 100 tabel per schema; hasil membaca metadata per tabel, sehingga perubahan schema saat perbandingan berlangsung dapat memengaruhi hasil. Tinjau SQL sebelum execute.

Login sebagai user dengan grant `edit`, buka Draft dari Dashboard, lalu simpan SQL yang ingin dijalankan. Pada Draft tersimpan, tinjau SQL, revision dan target, klik **Execute Migration**, kemudian konfirmasi. Tombol Execute dinonaktifkan jika belum tersimpan, ada perubahan lokal yang belum disimpan, sedang memproses permintaan, atau permintaan untuk revision tersebut sudah dikirim.

FE memanggil `POST /api/v1/databases/{id}/drafts/{draftID}/execute` dengan JSON `{ "revision": "N" }` dan CSRF sesi. Hasil BE2 dibaca dari `status`, bukan hanya kode HTTP: `success` = commit, `failed` = rollback, sedangkan `running`/`unknown` **belum final**. Tampilkan version, execution ID dan error code, lalu periksa History; tidak ada retry otomatis pada error jaringan atau status ambigu.

Sebelum pengujian live, gunakan **database target disposable yang bukan metadata**. Role target harus executor restricted (bukan superuser atau owner berlebih), dan admin PostgreSQL harus menjalankan `BE/sql/provision_target_history.sql` pada target sesuai petunjuk BE. Jangan menjalankan script provisioning itu melalui UI Draft. Tes SQL gagal pada target disposable untuk membuktikan rollback.

`409 target_history_unprotected` berarti ledger belum aman, bukan masalah tombol FE. `409 execution_conflict`/`migration_busy` membutuhkan pemeriksaan History; `409 target_identity_conflict` membutuhkan bantuan admin target. Jika CSRF/session expired, login ulang; jangan mengirim ulang eksekusi sebelum mengetahui outcome.


### history, reconcile, and cursor pagination

- Migration History uses `limit=50` plus BE2 `next_before`/`has_more` and includes version,
  draft ID/revision, execution ID, actor, timestamps, status, and error code. SQL is fetched
  only when opening the version detail. `running`/`unknown` are **not** final outcomes.
- A user with **Edit** grant can explicitly confirm **Reconcile Pending / Unknown**.
  This calls `POST /api/v1/databases/{id}/migrations/reconcile` and shows backend results;
  the FE never retries execution and never infers commit from HTTP status alone.
  **View Only** can read history but cannot trigger reconcile.
- Draft listing uses `limit=50` plus `next_after`/`has_more`; both lists support previous/next
  pagination. Identifiers and cursors stay as JSON strings (no unsafe JS number conversions).
- Test with `node --test` from `FE/` and `go test ./...`, `go vet ./...`;
  confirm behavior in a browser with the live backend and a disposable PostgreSQL target.
