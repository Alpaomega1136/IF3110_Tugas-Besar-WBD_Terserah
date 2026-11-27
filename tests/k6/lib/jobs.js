import { sleep } from 'k6';

const terminal = ['success', 'failed', 'unknown', 'partial', 'resumed', 'partially_reverted'];

export function enqueue(api, fixture, tags = {}) {
    const base = `/api/v1/databases/${fixture.databaseId}/jobs`;
    const result = api.request('POST', base, { review_id: String(fixture.reviewId) }, [202], 'job_enqueue', tags);
    if (result.replayed !== false || result.job.review_id !== fixture.reviewId) throw new Error('Expected fresh job for unique review');
    return result.job;
}

export function waitForJob(api, fixture, jobId, timeoutSeconds = 90, tags = {}) {
    const base = `/api/v1/databases/${fixture.databaseId}/jobs/${jobId}`;
    const deadline = Date.now() + timeoutSeconds * 1000;
    let latest;
    do {
        latest = api.request('GET', base, null, [200], 'job_status', tags);
        if (terminal.includes(latest.job.status)) return latest;
        sleep(0.5);
    } while (Date.now() < deadline);
    throw new Error(`Job ${jobId} timed out (last status ${latest.job.status})`);
}

export function jobTiming(job) {
    const created = Date.parse(job.created_at);
    const started = Date.parse(job.started_at);
    const finished = Date.parse(job.finished_at);
    if (![created, started, finished].every(Number.isFinite) || started < created || finished < started) {
        throw new Error(`Invalid job timestamps for job ${job.id}`);
    }
    return { queueWaitMs: started - created, executionMs: finished - started, endToEndMs: finished - created };
}
