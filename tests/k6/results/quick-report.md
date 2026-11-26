# k6 stress test — quick

Run ID: `mwxheavc_73a9e3`. Planned jobs: 15.

## HTTP workload

| Level | API VU target | Window (s) | Requests | req/s | p50 (ms) | p95 (ms) | HTTP error (%) | Threshold |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| normal | 2 | 20 | 276 | 13.80 | 7.07 | 90.15 | 0.00 | PASS |
| medium | 4 | 20 | 566 | 28.30 | 9.00 | 71.99 | 0.00 | PASS |
| stress | 6 | 27 | 1124 | 41.63 | 8.26 | 66.55 | 0.00 | PASS |
| recovery | 2 | 20 | 357 | 17.85 | 6.47 | 64.17 | 0.00 | PASS |

## Migration throughput by observation phase

| Level | Attempted | Accepted | Terminal observed | Success observed | Non-success | Unresolved events | Terminal/s | Success/s | Queue p50/p95 (ms) | Duration p50/p95 (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|
| normal | 3 | 3 | 3 | 3 | 0 | 0 | 0.15 | 0.15 | 204.00 / 810.60 | 701.00 / 714.50 |
| medium | 4 | 4 | 4 | 4 | 0 | 0 | 0.20 | 0.20 | 475.00 / 798.35 | 633.00 / 637.55 |
| stress | 5 | 5 | 5 | 5 | 0 | 0 | 0.19 | 0.19 | 797.00 / 1336.20 | 640.00 / 726.60 |
| recovery | 3 | 3 | 3 | 3 | 0 | 0 | 0.15 | 0.15 | 185.00 / 630.50 | 624.00 / 699.60 |
| drain | 0 | 0 | 0 | 0 | 0 | 0 | N/A | N/A | N/A / N/A | N/A / N/A |

## Jobs by submission cohort

| Level | Planned A/B | Accepted | Terminal | Success | Retries | Queue p50/p95 (ms) | Duration p50/p95 (ms) |
|---|---|---:|---:|---:|---:|---|---|
| normal | 2/1 | 3 | 3 | 3 | 0 | 204.00 / 810.60 | 701.00 / 714.50 |
| medium | 2/2 | 4 | 4 | 4 | 0 | 475.00 / 798.35 | 633.00 / 637.55 |
| stress | 3/2 | 5 | 5 | 5 | 0 | 797.00 / 1336.20 | 640.00 / 726.60 |
| recovery | 2/1 | 3 | 3 | 3 | 0 | 185.00 / 630.50 | 624.00 / 699.60 |

## Concurrency evidence (successful jobs, one attempt)

- Jobs with comparable timestamps: 15.
- Same-target overlapping pairs: 0.
- Different-target overlapping pairs: 10.
- Example different-target pairs: [('4', '5'), ('7', '8'), ('8', '9'), ('9', '10'), ('11', '12')].

## Final audit

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

## Method and limitations

- HTTP metrics include authenticated API reads, login/logout, enqueue and polling within each measured phase; setup and final audit are excluded. Threshold: p95 < 1000 ms and HTTP error rate < 5%.
- Request tags are set at request start; job counts are attributed to the time their terminal status is observed. A job can finish in a later phase than its submission cohort. Throughput is count / phase duration, including ramp; drain has no fixed duration.
- Queue time = first start minus creation; duration = final finish minus first start (includes retry delays if any). Only jobs with valid start/finish timestamps contribute timing samples.
- Percentiles use linear interpolation on raw k6 samples; missing measurements are N/A. HTTP threshold table is calculated from raw samples; the k6 exit status is authoritative.
- Final audit is independent from load metrics. Non-success or unresolved jobs cannot be counted as successful migrations. Concurrency pairs describe worker job timestamp intervals, not precise SQL lock hold intervals.
- Fill in the hardware/software environment and interpretation of each level in the root README based on these actual results.
