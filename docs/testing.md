# Pengujian

## Unit dan build

Dari folder `BE/`:

```powershell
go test ./...
go vet ./...
go build ./...
```

Unit test fondasi mencakup konfigurasi, health sukses/gagal tanpa kebocoran error driver, serta grant multi-group dan penolakan akses. Test permission ini belum membuktikan authorization endpoint fitur karena endpoint tersebut belum ada.

## Integration dengan PostgreSQL nyata

Buat user dan database pengujian sekali pada PostgreSQL lokal menggunakan akun administrator (contoh: `postgres`). User test memiliki database sendiri agar dapat membuat schema terisolasi:

```powershell
psql -h localhost -U postgres -d postgres -c "CREATE ROLE test LOGIN PASSWORD 'test_only';"
createdb -h localhost -U postgres -O test platform_test
```

Kemudian, dari root repository:

```powershell
Set-Location BE
$env:TEST_DATABASE_URL = 'postgres://test:test_only@localhost:5432/platform_test?sslmode=disable'
go test -tags=integration ./tests/integration/... -count=1 -v
```

Test wajib diberi URL secara eksplisit dan akan gagal jika tidak tersedia. Pakai database khusus test. Test membuat schema unik, menjalankan migration dua kali untuk membuktikan idempotensi, memeriksa constraint, dan memanggil handler readiness dengan pool nyata dalam kondisi tersedia/tertutup. Schema dibersihkan sesudah test; tidak ada `TRUNCATE` database aplikasi.

Sesuaikan URL dengan port dan kredensial lokal. Di shell POSIX gunakan `export TEST_DATABASE_URL='...'`.

`go test ./...` tidak menjalankan integration test karena build tag. CI menjalankan keduanya menggunakan service PostgreSQL bawaan runner Ubuntu dan menambahkan race detector pada Linux. Di Windows, `-race` memerlukan toolchain C yang mendukung; unit test normal tidak memerlukannya.

## Test yang wajib ditambahkan bersama fitur

| Modul | Skenario minimum |
| --- | --- |
| Auth/session | Login valid/invalid, hash password, idle/absolute expiry, rotasi/grace, concurrency, logout, revokasi, CSRF |
| Admin/policy | Non-admin ditolak, grant lintas database, perubahan membership, pencabutan dan peningkatan privilege |
| Koneksi/viewer | Dua target terpisah, kredensial salah, target mati, nama identifier khusus, pagination, baca tanpa izin |
| Draft/history | View Only ditolak mutasi, akses silang database ditolak, SQL history tidak dapat diubah |
| Runner | Commit sukses, rollback kegagalan statement tengah, versi linear bersamaan, histori gagal, commit ambigu/reconciliation |

Gunakan clock yang bisa dikontrol untuk test session. Tambahkan fixture kecil di `BE/tests/fixtures/`, bukan dump database pribadi. E2E lintas FE-BE berada di `tests/e2e/` setelah FE tersedia.

## Verifikasi fondasi (20 Oktober 2026)

Lulus dengan Go 1.26.1 di Windows: `go test ./... -count=1`, `go vet ./...`, `go build ./...`, dan integration test pada cluster PostgreSQL sementara terpisah. CLI migration dijalankan dua kali dengan sukses; server HTTP nyata mengembalikan 200/status `ok` pada kedua health endpoint. Format Go dan tautan Markdown lokal juga diperiksa.

Workflow GitHub Actions belum dijalankan di GitHub. Test lokal tidak berarti fitur backlog yang belum tersedia sudah teruji.

