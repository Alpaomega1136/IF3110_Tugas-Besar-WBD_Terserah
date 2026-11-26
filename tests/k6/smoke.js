import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { requester } from './lib/client.js';
import { prepareFixtures } from './lib/fixtures.js';
import { enqueue, waitForJob, jobTiming } from './lib/jobs.js';

const success = new Counter('smoke_jobs_success');
const queueWait = new Trend('smoke_queue_wait_ms', true);
const duration = new Trend('smoke_job_duration_ms', true);

export const options = {
    vus: 1,
    iterations: 1,
    setupTimeout: '5m',
    thresholds: {
        checks: ['rate==1'],
        http_req_failed: ['rate==0'],
        smoke_jobs_success: ['count==3'],
    },
};

export function setup() { return prepareFixtures(2, 1); }

export default function (data) {
    const { api } = requester('smoke');
    try {
        for (const fixture of data.reviews) {
            const job = enqueue(api, fixture);
            const result = waitForJob(api, fixture, job.id);
            if (!check(result.job, { 'migration reaches success': j => j.status === 'success' })) {
                throw new Error(`Job ${job.id}: ${result.job.status}, code=${result.job.error_code || 'none'}`);
            }
            const timing = jobTiming(result.job);
            success.add(1);
            queueWait.add(timing.queueWaitMs);
            duration.add(timing.executionMs);
            console.log(`target=${fixture.target} review=${fixture.reviewId} job=${job.id} status=success queue_ms=${timing.queueWaitMs} execution_ms=${timing.executionMs}`);
        }
    } finally { api.logout(); }
}
