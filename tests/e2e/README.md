# Pengujian end-to-end

Tempat pengujian lintas FE dan BE setelah keduanya tersedia. Framework belum dipilih agar mengikuti stack FE.

Skenario minimum: admin login -> user/group/grant -> pilih database -> lihat schema/data -> buat draft -> execute sukses -> history; ulangi dengan SQL gagal untuk rollback, View Only untuk penolakan mutasi, dan grant dicabut saat session aktif.

Unit/integration test backend berada di `BE/`, bukan di folder ini.

