# Pembagian kerja dan backlog

Referensi: spesifikasi resmi dan rencana kerja di `refs/specifications/`. Nama pemilik mengikuti peran, bukan nama anggota tertentu.

| Bagian | Pemilik | Status awal |
| --- | --- | --- |
| Struktur repo, Go server, metadata schema, CI | Bersama | Fondasi tersedia |
| Login, session, CSRF, bootstrap admin | BE 1 | Belum diimplementasikan |
| User, group, membership, grant dan invalidasi session | BE 1 | Belum diimplementasikan |
| Perhitungan akses efektif | BE 1 | Fungsi policy + unit test tersedia; wiring DB/API belum |
| Draft migration dan API history | BE 1 | Belum diimplementasikan |
| Koneksi target dan encrypted credential storage | BE 2 | Belum diimplementasikan |
| Viewer schema/table/column/constraint/index/data | BE 2 | Belum diimplementasikan |
| Runner, transaction, linear version, target history | BE 2 | Belum diimplementasikan |
| UI dan integrasi API | FE | Folder disediakan |
| Unit/integration test fitur | Pemilik tiap BE | Test fondasi tersedia; test fitur mengikuti implementasi |
| E2E dan demo | Bersama | Skenario awal didokumentasikan |

Urutan: sepakati kontrak -> auth/session -> admin/grant -> koneksi/viewer -> draft -> runner/history -> integrasi. Setiap fitur perlu alur berhasil, gagal, dan akses ditolak sebelum ditandai selesai.

Bonus (execution plan, schema diff/generation, DBMS tambahan) belum dibuat. Prioritaskan alur wajib dan pengujiannya terlebih dahulu.

