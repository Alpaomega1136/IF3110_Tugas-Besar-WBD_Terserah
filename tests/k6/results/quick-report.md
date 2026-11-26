# Pengujian beban dengan k6 — quick

ID pengujian: `mwxheavc_73a9e3`. Jumlah job yang direncanakan: 15.

## Beban HTTP

| Tingkat beban | Target VU API | Durasi (detik) | Jumlah request | Request/detik | p50 (ms) | p95 (ms) | Kesalahan HTTP (%) | Ambang batas |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| Normal | 2 | 20 | 276 | 13.80 | 7.07 | 90.15 | 0.00 | LULUS |
| Menengah | 4 | 20 | 566 | 28.30 | 9.00 | 71.99 | 0.00 | LULUS |
| Stress | 6 | 27 | 1124 | 41.63 | 8.26 | 66.55 | 0.00 | LULUS |
| Pemulihan | 2 | 20 | 357 | 17.85 | 6.47 | 64.17 | 0.00 | LULUS |

## Throughput migration berdasarkan fase pengamatan

| Tingkat beban | Dicoba | Diterima | Mencapai status terminal | Berhasil | Tidak berhasil | Belum terkonfirmasi | Terminal/detik | Berhasil/detik | Waktu antre p50/p95 (ms) | Durasi p50/p95 (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| Normal | 3 | 3 | 3 | 3 | 0 | 0 | 0.15 | 0.15 | 204.00 / 810.60 | 701.00 / 714.50 |
| Menengah | 4 | 4 | 4 | 4 | 0 | 0 | 0.20 | 0.20 | 475.00 / 798.35 | 633.00 / 637.55 |
| Stress | 5 | 5 | 5 | 5 | 0 | 0 | 0.19 | 0.19 | 797.00 / 1336.20 | 640.00 / 726.60 |
| Pemulihan | 3 | 3 | 3 | 3 | 0 | 0 | 0.15 | 0.15 | 185.00 / 630.50 | 624.00 / 699.60 |
| Penyelesaian antrean | 0 | 0 | 0 | 0 | 0 | 0 | N/A | N/A | N/A / N/A | N/A / N/A |

## Job berdasarkan fase pengiriman

| Tingkat beban | Rencana A/B | Diterima | Terminal | Berhasil | Percobaan ulang | Waktu antre p50/p95 (ms) | Durasi p50/p95 (ms) |
|---|---|---:|---:|---:|---:|---|---|
| Normal | 2/1 | 3 | 3 | 3 | 0 | 204.00 / 810.60 | 701.00 / 714.50 |
| Menengah | 2/2 | 4 | 4 | 4 | 0 | 475.00 / 798.35 | 633.00 / 637.55 |
| Stress | 3/2 | 5 | 5 | 5 | 0 | 797.00 / 1336.20 | 640.00 / 726.60 |
| Pemulihan | 2/1 | 3 | 3 | 3 | 0 | 185.00 / 630.50 | 624.00 / 699.60 |

## Bukti konkurensi (job berhasil dalam satu percobaan)

- Job dengan timestamp yang dapat dibandingkan: 15.
- Pasangan job yang bertumpang tindih pada target yang sama: 0.
- Pasangan job yang bertumpang tindih pada target berbeda: 10.
- Contoh pasangan ID job pada target berbeda: [('4', '5'), ('7', '8'), ('8', '9'), ('9', '10'), ('11', '12')].

## Audit akhir

```text
K6_AUDIT review=4 target=a job=6 status=success
K6_AUDIT review=5 target=a job=4 status=success
K6_AUDIT review=6 target=a job=9 status=success
K6_AUDIT review=7 target=a job=7 status=success
K6_AUDIT review=8 target=a job=12 status=success
K6_AUDIT review=9 target=a job=15 status=success
K6_AUDIT review=10 target=a job=14 status=success
K6_AUDIT review=11 target=a job=16 status=success
K6_AUDIT review=12 target=a job=18 status=success
K6_AUDIT review=13 target=b job=5 status=success
K6_AUDIT review=14 target=b job=8 status=success
K6_AUDIT review=15 target=b job=10 status=success
K6_AUDIT review=16 target=b job=13 status=success
K6_AUDIT review=17 target=b job=11 status=success
K6_AUDIT review=18 target=b job=17 status=success
```

## Metodologi dan batasan

- Metrik HTTP mencakup pembacaan API terautentikasi, login/logout, pengiriman job ke antrean, dan polling pada setiap fase pengukuran. Setup dan audit akhir tidak disertakan. Ambang batas: p95 < 1000 ms dan tingkat kesalahan HTTP < 5%.
- Tag request ditetapkan saat request dimulai. Jumlah job dihitung berdasarkan fase ketika status terminalnya teramati. Job dapat selesai pada fase yang berbeda dari fase pengirimannya. Throughput dihitung dengan membagi jumlah request atau job dengan durasi fase, termasuk perubahan beban (ramp). Fase penyelesaian antrean (drain) tidak memiliki durasi tetap.
- Waktu antre dihitung dari waktu pembuatan job hingga pertama kali dimulai. Durasi dihitung dari waktu mulai pertama hingga selesai (termasuk jeda percobaan ulang jika ada). Hanya job dengan timestamp mulai dan selesai yang valid yang digunakan dalam pengukuran waktu.
- Persentil dihitung menggunakan interpolasi linear pada sampel mentah k6. N/A menunjukkan bahwa data pengukuran tidak tersedia atau metrik tidak berlaku. Tabel ambang batas HTTP dihitung dari sampel mentah; status keluar k6 menjadi acuan utama.
- Audit akhir dilakukan secara terpisah dari metrik beban. Job yang tidak berhasil atau belum terkonfirmasi tidak dihitung sebagai migration berhasil. Pasangan konkurensi menggambarkan interval timestamp job pada worker, bukan interval kepemilikan SQL lock secara langsung.
- Lingkungan pengujian dan analisis hasil setiap tingkat beban didokumentasikan pada bagian Stress Testing dengan k6 di README utama repositori.
