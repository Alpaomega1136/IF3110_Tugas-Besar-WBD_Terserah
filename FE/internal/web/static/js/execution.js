const int64Pattern = /^[1-9][0-9]*$/;
const validVersion = (value) => typeof value === "string" && int64Pattern.test(value) &&
    BigInt(value) <= 9223372036854775807n;

export function parseExecutionResult(result) {
    if (!result || typeof result.execution_id !== "string" || !result.execution_id ||
        !validVersion(result.version) ||
        !["success", "failed", "running", "unknown"].includes(result.status) ||
        (result.error_code != null && typeof result.error_code !== "string")) {
        throw new Error("Respons eksekusi tidak valid. Hasil tidak dapat dipastikan; periksa History.");
    }
    return result;
}

export function executionFeedback(status) {
    switch (status) {
        case "success": return { message: "Migration berhasil di-commit. Periksa Viewer dan History untuk memastikan perubahan.", isError: false };
        case "failed": return { message: "Migration gagal dan transaksi telah di-rollback. Lihat error code dan History sebelum memperbaiki draft.", isError: true };
        case "running": return { message: "Migration masih berjalan (belum final). Periksa History; jangan mengirim Execute lagi.", isError: false };
        case "unknown": return { message: "Hasil eksekusi belum pasti. Periksa History dan lakukan reconcile bila diperlukan; jangan menjalankan ulang.", isError: true };
        default: throw new Error("Status migration tidak dikenal.");
    }
}

export function executeErrorMessage(error) {
    if (error?.status === 403 && error.code === "csrf_rejected") {
        return "CSRF ditolak. Refresh halaman atau login ulang, lalu periksa History sebelum mencoba eksekusi kembali.";
    }
    if (error?.status === 409) {
        switch (error.code) {
            case "execution_conflict": return "Revisi berubah atau ada eksekusi pending. Buka ulang draft dan periksa History sebelum tindakan berikutnya.";
            case "migration_busy": return "Database target sedang menjalankan migration lain. Periksa History sebelum mencoba lagi secara manual.";
            case "target_history_unprotected": return "History target atau isolasi executor belum aman. Minta admin PostgreSQL menjalankan BE/sql/provision_target_history.sql dan gunakan executor restricted khusus database ini, lalu buka ulang draft.";
            case "target_identity_conflict": return "Identitas database target berubah atau sudah terhubung ke koneksi lain. Hubungi Platform Admin; tidak ada bypass dari frontend.";
            case "metadata_target_forbidden": return "Database metadata platform tidak boleh dijadikan target migration.";
            default: return "Konflik eksekusi migration. Periksa History dan muat ulang draft sebelum mencoba lagi.";
        }
    }
    if (error?.status === 404) return "Draft atau database tidak ditemukan. Muat ulang halaman dari Dashboard.";
    if (error?.status === 503 || error?.status === 502 || !error?.status) {
        return "Backend atau target tidak tersedia, atau koneksi terputus. Hasil eksekusi mungkin belum pasti. Jangan retry otomatis; periksa Migration History.";
    }
    if (error?.status === 400) return "Eksekusi ditolak karena input atau SQL tidak didukung. Periksa draft tersimpan dan revision.";
    return "Eksekusi tidak dapat dipastikan. Periksa Migration History sebelum mencoba lagi.";
}
