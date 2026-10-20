# Kontrak API

## Sudah tersedia

| Method | Path | Sukses | Gagal |
| --- | --- | --- | --- |
| GET | `/health/live` | 200 `{"status":"ok"}` | - |
| GET | `/health/ready` | 200 `{"status":"ok"}` | 503 `database_unavailable` |

Readiness memeriksa koneksi metadata, belum memeriksa kelengkapan schema atau koneksi target. Path tak dikenal mendapat 404. Response health tidak memuat detail error driver/kredensial.

Format error aplikasi: `{"error":{"code":"not_found","message":"Endpoint not found"}}`.
Status rencana: 400 input salah, 401 belum login/session kedaluwarsa, 403 akses/CSRF ditolak, 404 objek tidak ditemukan, 409 konflik versi/status, 503 target tidak tersedia. Respons bawaan method tidak didukung dari `net/http` dapat berupa teks.

## Rencana endpoint (BELUM tersedia)

Gunakan prefix `/api/v1`. Tabel ini menjadi bahan kesepakatan tim, bukan janji endpoint yang sudah berfungsi.

| Modul | Method dan path usulan | Akses |
| --- | --- | --- |
| Auth | `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`, `GET /auth/csrf` | Sesuai alur session/CSRF |
| Admin | `GET/POST /admin/users`, `PATCH /admin/users/{id}` | Platform Admin |
| Admin | `GET/POST /admin/groups`, `PUT/DELETE /admin/groups/{id}/users/{userID}` | Platform Admin |
| Grant | `PUT/DELETE /admin/groups/{id}/databases/{databaseID}` | Platform Admin |
| Koneksi | `GET/POST /admin/databases`, `PATCH/DELETE /admin/databases/{id}`, `POST /admin/databases/{id}/test` | Platform Admin |
| Database | `GET /databases` | Session; filter sesuai grant |
| Viewer | `GET /databases/{id}/schemas` | View atau Edit |
| Viewer | `GET /databases/{id}/schemas/{schema}/tables` | View atau Edit |
| Viewer | `GET /databases/{id}/schemas/{schema}/tables/{table}` | View atau Edit; metadata column/constraint/index |
| Viewer | `GET /databases/{id}/schemas/{schema}/tables/{table}/rows?limit=50&offset=0` | View atau Edit |
| Draft | `POST /databases/{id}/drafts`, `PATCH /databases/{id}/drafts/{draftID}` | Edit |
| Execute | `POST /databases/{id}/drafts/{draftID}/execute` | Edit |
| History | `GET /databases/{id}/migrations`, `GET /databases/{id}/migrations/{version}` | View atau Edit |

Sebelum FE memakai mock fitur, sepakati bentuk payload, ID (disarankan string pada JSON untuk bigint), pagination, field error validasi, status migration, dan alur CSRF bersama pemilik BE. Seluruh mutasi memerlukan perlindungan CSRF; jangan menerima `user_id` client sebagai identitas pelaksana.

