# Arsitektur dan keputusan awal

Ketentuan wajib bersumber dari PDF spesifikasi Milestone 1 di `refs/specifications/`. PDF rencana kerja adalah bahan pembagian kerja; label USULAN di dalamnya bukan kewajiban tambahan. Keputusan berikut merupakan default awal tim dan dapat diubah secara konsisten bersama test serta dokumentasinya.

## Batas modul

Alur rencana: HTTP handler -> validasi session/CSRF -> policy -> service modul -> repository PostgreSQL atau koneksi target.
Gunakan `net/http` bawaan Go dan `pgx/v5`; tambah dependency saat ada kebutuhan nyata. Gunakan module `terserah/backend` untuk import internal; tidak perlu menunggu URL GitHub.

- Metadata platform menyimpan user, group, grant, session, koneksi, draft dan ledger eksekusi.
- Target PostgreSQL adalah database yang dijelajahi/diubah pengguna, terpisah dari metadata. Jangan mengeksekusi SQL pengguna pada koneksi metadata.
- `BE/migrations/` hanya schema metadata. `BE/internal/migration/` untuk fitur migration target.
- Unit test diletakkan bersama file Go. Integration test lintas modul di `BE/tests/integration/`. Jangan buat folder test terpisah untuk setiap fungsi kecil.
- `internal/httpapi` tempat wiring route/middleware; handler dan query fitur dapat diletakkan di modul pemiliknya saat dikembangkan.

## Policy yang sudah tersedia

`policy.Effective` menerima grant dari group **untuk satu database**. Hasilnya izin tertinggi: tidak ada < `view` < `edit`. Nilai tak dikenal ditolak. `policy.Allows` menentukan baca/tulis. Query pengambilan grant dan middleware authorization belum tersedia.
Platform Admin mengelola platform; akses target tetap melalui group. Setiap handler target wajib menghitung izin dari principal/session dan database ID pada request, bukan dari input role client.

## Kontrak keamanan yang perlu diimplementasikan

- Hash password menggunakan algoritma password hashing yang sesuai; jangan mengisi `password_hash` dengan plaintext.
- Opaque session identifier acak melalui cookie `terserah_session`, HttpOnly, SameSite=Lax, Path=/, Secure pada HTTPS/production. Jangan menaruh identifier di JSON, URL, atau log.
- Default usulan: idle 30 menit, absolute 8 jam, rotasi 15 menit, grace 30 detik. Rotasi tidak mengubah absolute expiry.
- Tabel `session_tokens` menyimpan hash identifier dan masa berlaku alias. Revokasi record session membatalkan seluruh token terkait.
- Rotasi pada login/peningkatan privilege; pencabutan akses merevokasi session terdampak. Lakukan perubahan grant dan invalidasi/rotasi dalam transaksi metadata yang sama.
- Semua mutasi perlu CSRF, termasuk pertahanan login CSRF. Sepakati penerbitan token, header `X-CSRF-Token`, dan validasi Origin. Cookie session saja tidak cukup.
- `password_encrypted` menampung ciphertext kredensial target. Implementasikan authenticated encryption dan simpan key di secret environment, terpisah dari database. Jangan kirim password/DSN ke response.
- Deployment satu origin disarankan. Cross-origin memerlukan allowlist CORS eksplisit dan credential handling; belum diaktifkan dalam fondasi.

## Konsistensi migration target (rancangan, belum diimplementasikan)

Gunakan transaction dan advisory lock pada target, dengan versi linear per database. Catat keberhasilan pada tabel history khusus target di transaksi yang sama dengan SQL pengguna; ledger metadata bukan bukti commit target. Setelah commit, sinkronkan ledger metadata. Jika koneksi terputus saat commit, tandai `unknown` dan rekonsiliasi melalui history target sebelum retry. Catat kegagalan sesudah rollback agar tidak ikut hilang.

Jangan menganggap transaksi metadata dan target sebagai satu transaksi atomik. Tolak transaction-control SQL pengguna dan operasi yang tidak mendukung transaction. Jangan memisahkan SQL dengan `strings.Split(";")`; pilih parser/strategi eksekusi yang benar saat runner dibangun. Tetapkan timeout, batas ukuran SQL, serta aturan hak database target sebelum endpoint eksekusi diaktifkan.

