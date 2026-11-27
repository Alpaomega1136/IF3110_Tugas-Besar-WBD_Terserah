import { positiveInt64 } from "./pagination.js";

const validCount = (value) => value === "0" || positiveInt64(value);
const validProgress = (item) => ["committed_batches", "committed_rows"].every((field) => item[field] == null || validCount(item[field]));

export function parseExecutionResult(result) {
    if (!result || typeof result.execution_id !== "string" || !result.execution_id ||
        !positiveInt64(result.version) ||
        !["success", "failed", "running", "unknown", "partial", "partially_reverted"].includes(result.status) || !validProgress(result) ||
        (result.error_code != null && typeof result.error_code !== "string")) {
        throw new Error("Respons eksekusi tidak valid. Hasil tidak dapat dipastikan; periksa History.");
    }
    return result;
}

export function parseJobResponse(response, databaseID) {
    const job = response?.job;
    if (!job || !positiveInt64(job.id) || job.database_id !== databaseID ||
        !["queued", "running", "success", "failed", "unknown", "partial", "recovering", "retry_wait", "resumed", "partially_reverted"].includes(job.status) || !validProgress(job) ||
        ["parent_job_id", "resumed_by_job_id"].some((field) => job[field] != null && !positiveInt64(job[field])) ||
        (job.status === "resumed" && !positiveInt64(job.resumed_by_job_id)) ||
        (job.target_execution_id != null && (typeof job.target_execution_id !== "string" || !job.target_execution_id)) ||
        (job.available_at != null && (typeof job.available_at !== "string" || !Number.isFinite(Date.parse(job.available_at)))) ||
        ["attempt_count", "max_attempts"].some((field) => job[field] != null && (!Number.isSafeInteger(job[field]) || job[field] < (field === "max_attempts" ? 1 : 0))) ||
        (job.error_code != null && typeof job.error_code !== "string")) {
        throw new Error("Respons job tidak valid. Periksa History sebelum mengirim ulang.");
    }
    return job;
}

export function executionFeedback(status) {
    switch (status) {
        case "queued": return { message: "Migration masuk antrean. SQL belum dinyatakan berhasil; worker akan memprosesnya.", isError: false };
        case "success": return { message: "Migration berhasil di-commit. Periksa Viewer dan History untuk memastikan perubahan.", isError: false };
        case "failed": return { message: "Migration gagal dan transaksi telah di-rollback. Lihat error code dan History sebelum memperbaiki draft.", isError: true };
        case "partially_reverted": return { message: "Pembatalan batch belum selesai. Periksa jumlah batch/row yang sudah dipulihkan; target ditahan sampai kompensasi dilanjutkan dengan approval baru.", isError: true };
        case "partial": return { message: "Sebagian batch sudah di-commit. Batch yang gagal dibatalkan; batch sebelumnya tetap tersimpan. Target ditahan sampai pemulihan yang disetujui. Jangan Execute ulang.", isError: true };
        case "running": return { message: "Migration masih berjalan (belum final). Periksa History; jangan mengirim Execute lagi.", isError: false };
        case "recovering": return { message: "Worker memeriksa receipt dan checkpoint di target sebelum menentukan retry. Batch yang sudah committed tetap tersimpan. Jangan Execute ulang.", isError: false };
        case "retry_wait": return { message: "Job menunggu jadwal retry atau resume yang disetujui. Percobaan berikutnya memiliki execution ID baru; batch yang committed akan dilewati. Jangan Execute ulang.", isError: false };
        case "resumed": return { message: "Pemulihan dialihkan ke job terkait. History percobaan lama tetap tersimpan; periksa job pemulihan untuk hasil kelanjutannya.", isError: false };
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
            case "approval_incomplete": return "Approval belum lengkap atau ada perubahan leader dan akses. Ajukan review baru jika diperlukan.";
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
