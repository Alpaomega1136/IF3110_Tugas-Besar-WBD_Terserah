# Frontend

Folder ini dimiliki pengembang frontend. Framework, package manager, dan isi aplikasi belum ditentukan.

- Letakkan project frontend langsung di `FE/`.
- Backend development: `http://localhost:8080`.
- Gunakan proxy development `/api` dan `/health` ke backend agar request cookie tetap same-origin; deployment juga memakai satu origin.
- Kontrak yang sudah tersedia: [API](../docs/api.md). Endpoint fitur lainnya masih direncanakan.
- Saat auth diimplementasikan, gunakan cookie session HttpOnly dan header CSRF sesuai kontrak bersama BE. Jangan simpan session identifier di localStorage.
- Tambahkan perintah run/build/test frontend di README ini setelah stack dipilih.

