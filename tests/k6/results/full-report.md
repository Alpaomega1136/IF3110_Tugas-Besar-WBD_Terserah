# k6 stress test — full

Run ID: `mwxinlbu_332f34`. Planned jobs: 32.

## HTTP workload

| Level | API VU target | Window (s) | Requests | req/s | p50 (ms) | p95 (ms) | HTTP error (%) | Threshold |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| normal | 10 | 45 | 2904 | 64.53 | 9.70 | 83.52 | 0.00 | PASS |
| medium | 25 | 60 | 8075 | 134.58 | 44.35 | 167.80 | 0.00 | PASS |
| stress | 50 | 75 | 12377 | 165.03 | 155.79 | 431.10 | 0.00 | PASS |
| recovery | 10 | 45 | 3390 | 75.33 | 11.71 | 180.03 | 0.00 | PASS |

## Migration throughput by observation phase

| Level | Attempted | Accepted | Terminal observed | Success observed | Non-success | Unresolved events | Terminal/s | Success/s | Queue p50/p95 (ms) | Duration p50/p95 (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| normal | 4 | 4 | 4 | 4 | 0 | 0 | 0.09 | 0.09 | 469.00 / 872.20 | 688.00 / 745.60 |
| medium | 8 | 8 | 8 | 8 | 0 | 0 | 0.13 | 0.13 | 2471.00 / 4951.10 | 998.00 / 1303.60 |
| stress | 16 | 16 | 16 | 16 | 0 | 0 | 0.21 | 0.21 | 7042.00 / 16936.75 | 1434.00 / 1559.25 |
| recovery | 4 | 4 | 4 | 4 | 0 | 0 | 0.09 | 0.09 | 451.00 / 845.85 | 740.00 / 767.70 |
| drain | 0 | 0 | 0 | 0 | 0 | 0 | N/A | N/A | N/A / N/A | N/A / N/A |

## Jobs by submission cohort

| Level | Planned A/B | Accepted | Terminal | Success | Retries | Queue p50/p95 (ms) | Duration p50/p95 (ms) |
|---|---|---:|---:|---:|---:|---|---|
| normal | 2/2 | 4 | 4 | 4 | 0 | 469.00 / 872.20 | 688.00 / 745.60 |
| medium | 5/3 | 8 | 8 | 8 | 0 | 2471.00 / 4951.10 | 998.00 / 1303.60 |
| stress | 12/4 | 16 | 16 | 16 | 0 | 7042.00 / 16936.75 | 1434.00 / 1559.25 |
| recovery | 2/2 | 4 | 4 | 4 | 0 | 451.00 / 845.85 | 740.00 / 767.70 |

## Concurrency evidence (successful jobs, one attempt)

- Jobs with comparable timestamps: 32.
- Same-target overlapping pairs: 0.
- Different-target overlapping pairs: 16.
- Example different-target pairs: [('19', '20'), ('21', '22'), ('24', '23'), ('23', '26'), ('25', '26')].

## Final audit

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

## Method and limitations

- HTTP metrics include authenticated API reads, login/logout, enqueue and polling within each measured phase; setup and final audit are excluded. Threshold: p95 < 1000 ms and HTTP error rate < 5%.
- Request tags are set at request start; job counts are attributed to the time their terminal status is observed. A job can finish in a later phase than its submission cohort. Throughput is count / phase duration, including ramp; drain has no fixed duration.
- Queue time = first start minus creation; duration = final finish minus first start (includes retry delays if any). Only jobs with valid start/finish timestamps contribute timing samples.
- Percentiles use linear interpolation on raw k6 samples; missing measurements are N/A. HTTP threshold table is calculated from raw samples; the k6 exit status is authoritative.
- Final audit is independent from load metrics. Non-success or unresolved jobs cannot be counted as successful migrations. Concurrency pairs describe worker job timestamp intervals, not precise SQL lock hold intervals.
- Fill in the hardware/software environment and interpretation of each level in the root README based on these actual results.
