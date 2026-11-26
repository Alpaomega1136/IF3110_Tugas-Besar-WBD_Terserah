# Pengujian beban dengan k6 — full

ID pengujian: `mwxinlbu_332f34`. Jumlah job yang direncanakan: 32.

## Beban HTTP

| Tingkat beban | Target VU API | Durasi (detik) | Jumlah request | Request/detik | p50 (ms) | p95 (ms) | Kesalahan HTTP (%) | Ambang batas |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| Normal | 10 | 45 | 2904 | 64.53 | 9.70 | 83.52 | 0.00 | LULUS |
| Menengah | 25 | 60 | 8075 | 134.58 | 44.35 | 167.80 | 0.00 | LULUS |
| Stress | 50 | 75 | 12377 | 165.03 | 155.79 | 431.10 | 0.00 | LULUS |
| Pemulihan | 10 | 45 | 3390 | 75.33 | 11.71 | 180.03 | 0.00 | LULUS |

## Throughput migration berdasarkan fase pengamatan

| Tingkat beban | Dicoba | Diterima | Mencapai status terminal | Berhasil | Tidak berhasil | Belum terkonfirmasi | Terminal/detik | Berhasil/detik | Waktu antre p50/p95 (ms) | Durasi p50/p95 (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| Normal | 4 | 4 | 4 | 4 | 0 | 0 | 0.09 | 0.09 | 469.00 / 872.20 | 688.00 / 745.60 |
| Menengah | 8 | 8 | 8 | 8 | 0 | 0 | 0.13 | 0.13 | 2471.00 / 4951.10 | 998.00 / 1303.60 |
| Stress | 16 | 16 | 16 | 16 | 0 | 0 | 0.21 | 0.21 | 7042.00 / 16936.75 | 1434.00 / 1559.25 |
| Pemulihan | 4 | 4 | 4 | 4 | 0 | 0 | 0.09 | 0.09 | 451.00 / 845.85 | 740.00 / 767.70 |
| Penyelesaian antrean | 0 | 0 | 0 | 0 | 0 | 0 | N/A | N/A | N/A / N/A | N/A / N/A |

## Job berdasarkan fase pengiriman

| Tingkat beban | Rencana A/B | Diterima | Terminal | Berhasil | Percobaan ulang | Waktu antre p50/p95 (ms) | Durasi p50/p95 (ms) |
|---|---|---:|---:|---:|---:|---|---|
| Normal | 2/2 | 4 | 4 | 4 | 0 | 469.00 / 872.20 | 688.00 / 745.60 |
| Menengah | 5/3 | 8 | 8 | 8 | 0 | 2471.00 / 4951.10 | 998.00 / 1303.60 |
| Stress | 12/4 | 16 | 16 | 16 | 0 | 7042.00 / 16936.75 | 1434.00 / 1559.25 |
| Pemulihan | 2/2 | 4 | 4 | 4 | 0 | 451.00 / 845.85 | 740.00 / 767.70 |

## Bukti konkurensi (job berhasil dalam satu percobaan)

- Job dengan timestamp yang dapat dibandingkan: 32.
- Pasangan job yang bertumpang tindih pada target yang sama: 0.
- Pasangan job yang bertumpang tindih pada target berbeda: 16.
- Contoh pasangan ID job pada target berbeda: [('19', '20'), ('21', '22'), ('24', '23'), ('23', '26'), ('25', '26')].

## Audit akhir

```text
K6_AUDIT review=19 target=a job=20 status=success
K6_AUDIT review=20 target=a job=22 status=success
K6_AUDIT review=21 target=a job=23 status=success
K6_AUDIT review=22 target=a job=29 status=success
K6_AUDIT review=23 target=a job=30 status=success
K6_AUDIT review=24 target=a job=25 status=success
K6_AUDIT review=25 target=a job=27 status=success
K6_AUDIT review=26 target=a job=41 status=success
K6_AUDIT review=27 target=a job=42 status=success
K6_AUDIT review=28 target=a job=33 status=success
K6_AUDIT review=29 target=a job=31 status=success
K6_AUDIT review=30 target=a job=35 status=success
K6_AUDIT review=31 target=a job=40 status=success
K6_AUDIT review=32 target=a job=39 status=success
K6_AUDIT review=33 target=a job=37 status=success
K6_AUDIT review=34 target=a job=43 status=success
K6_AUDIT review=35 target=a job=45 status=success
K6_AUDIT review=36 target=a job=44 status=success
K6_AUDIT review=37 target=a job=46 status=success
K6_AUDIT review=38 target=a job=48 status=success
K6_AUDIT review=39 target=a job=50 status=success
K6_AUDIT review=40 target=b job=19 status=success
K6_AUDIT review=41 target=b job=21 status=success
K6_AUDIT review=42 target=b job=24 status=success
K6_AUDIT review=43 target=b job=28 status=success
K6_AUDIT review=44 target=b job=26 status=success
K6_AUDIT review=45 target=b job=32 status=success
K6_AUDIT review=46 target=b job=38 status=success
K6_AUDIT review=47 target=b job=34 status=success
K6_AUDIT review=48 target=b job=36 status=success
K6_AUDIT review=49 target=b job=49 status=success
K6_AUDIT review=50 target=b job=47 status=success
```

## Metodologi dan batasan

- Metrik HTTP mencakup pembacaan API terautentikasi, login/logout, pengiriman job ke antrean, dan polling pada setiap fase pengukuran. Setup dan audit akhir tidak disertakan. Ambang batas: p95 < 1000 ms dan tingkat kesalahan HTTP < 5%.
- Tag request ditetapkan saat request dimulai. Jumlah job dihitung berdasarkan fase ketika status terminalnya teramati. Job dapat selesai pada fase yang berbeda dari fase pengirimannya. Throughput dihitung dengan membagi jumlah request atau job dengan durasi fase, termasuk perubahan beban (ramp). Fase penyelesaian antrean (drain) tidak memiliki durasi tetap.
- Waktu antre dihitung dari waktu pembuatan job hingga pertama kali dimulai. Durasi dihitung dari waktu mulai pertama hingga selesai (termasuk jeda percobaan ulang jika ada). Hanya job dengan timestamp mulai dan selesai yang valid yang digunakan dalam pengukuran waktu.
- Persentil dihitung menggunakan interpolasi linear pada sampel mentah k6. N/A menunjukkan bahwa data pengukuran tidak tersedia atau metrik tidak berlaku. Tabel ambang batas HTTP dihitung dari sampel mentah; status keluar k6 menjadi acuan utama.
- Audit akhir dilakukan secara terpisah dari metrik beban. Job yang tidak berhasil atau belum terkonfirmasi tidak dihitung sebagai migration berhasil. Pasangan konkurensi menggambarkan interval timestamp job pada worker, bukan interval kepemilikan SQL lock secara langsung.
- Lingkungan pengujian dan analisis hasil setiap tingkat beban didokumentasikan pada bagian Stress Testing dengan k6 di README utama repositori.
