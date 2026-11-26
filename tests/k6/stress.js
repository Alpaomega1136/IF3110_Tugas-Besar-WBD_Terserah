import exec from 'k6/execution';
import { sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { requester } from './lib/client.js';
import { prepareFixtures } from './lib/fixtures.js';
import { enqueue, jobTiming } from './lib/jobs.js';

const profile = __ENV.K6_PROFILE || 'full';
if (!['quick', 'full'].includes(profile)) throw new Error('K6_PROFILE must be quick or full');
// [phase, ramp seconds, hold seconds, API VUs, jobs A, jobs B]
const config = profile === 'quick' ? [
    ['normal', 3, 17, 2, 2, 1], ['medium', 3, 17, 4, 2, 2],
    ['stress', 3, 24, 6, 3, 2], ['recovery', 3, 17, 2, 2, 1],
] : [
    ['normal', 5, 40, 10, 2, 2], ['medium', 5, 55, 25, 5, 3],
    ['stress', 5, 70, 50, 12, 4], ['recovery', 5, 40, 10, 2, 2],
];
let elapsed = 0;
const phases = config.map(([name, ramp, hold, vus, a, b]) => {
    const phase = { name, ramp, hold, vus, a, b, offset: elapsed, duration: ramp + hold };
    elapsed += phase.duration;
    return phase;
});
const totalJobs = phases.reduce((sum, p) => sum + p.a + p.b, 0);
const metrics = {
    attempted: new Counter('stress_jobs_attempted'),
    accepted: new Counter('stress_jobs_accepted'),
    terminal: new Counter('stress_jobs_terminal'),
    success: new Counter('stress_jobs_success'),
    unsuccessful: new Counter('stress_jobs_unsuccessful'),
    unresolved: new Counter('stress_jobs_unresolved'),
    errors: new Counter('stress_workload_errors'),
    queue: new Trend('stress_queue_wait_ms', true),
    duration: new Trend('stress_job_duration_ms', true),
    endToEnd: new Trend('stress_job_e2e_ms', true),
    retries: new Counter('stress_job_retries'),
    auditSuccess: new Counter('stress_audit_success'),
};
const scenarios = {
    api: {
        executor: 'ramping-vus', exec: 'readAPI', startVUs: 0,
        stages: [...phases.flatMap(p => [
            { duration: `${p.ramp}s`, target: p.vus },
            { duration: `${p.hold}s`, target: p.vus },
        ]), { duration: '5s', target: 0 }],
        gracefulRampDown: '5s', gracefulStop: '15s',
    },
};
const thresholds = {
    stress_jobs_success: [`count==${totalJobs}`],
    stress_audit_success: [`count==${totalJobs}`],
};
for (const p of phases) {
    // One unique approved review per VU, with concurrent A and B submissions.
    scenarios[`jobs_${p.name}`] = {
        executor: 'per-vu-iterations', exec: 'submitJob', vus: p.a + p.b,
        iterations: 1, startTime: `${p.offset + p.ramp}s`,
        maxDuration: '160s', gracefulStop: '15s',
        env: { STRESS_COHORT: p.name },
    };
    thresholds[`http_req_duration{phase:${p.name}}`] = ['p(95)<1000'];
    thresholds[`http_req_failed{phase:${p.name}}`] = ['rate<0.05'];
}
export const options = {
    scenarios, thresholds, setupTimeout: '10m', teardownTimeout: '3m',
    noCookiesReset: true, summaryTrendStats: ['avg', 'med', 'p(95)', 'max'],
    // Avoid dynamic URL values in the raw metrics (stable endpoint name is tagged by client.js).
    systemTags: ['status', 'method', 'name', 'scenario', 'expected_response', 'check', 'error_code'],
};

function phaseAt(epoch) {
    const seconds = (Date.now() - epoch) / 1000;
    return phases.find(p => seconds < p.offset + p.duration)?.name || 'drain';
}

export function setup() {
    const data = prepareFixtures(phases.reduce((s, p) => s + p.a, 0), phases.reduce((s, p) => s + p.b, 0));
    const a = data.reviews.filter(r => r.target === 'a');
    const b = data.reviews.filter(r => r.target === 'b');
    const cohorts = {};
    for (const p of phases) {
        // Interleave target A and B in the assigned reviews.
        const selectedA = a.splice(0, p.a), selectedB = b.splice(0, p.b);
        cohorts[p.name] = [];
        for (let i = 0; i < Math.max(p.a, p.b); i++) {
            if (selectedA[i]) cohorts[p.name].push(selectedA[i]);
            if (selectedB[i]) cohorts[p.name].push(selectedB[i]);
        }
    }
    console.log(`K6_PROFILE ${JSON.stringify({ profile, phases, totalJobs, runId: data.runId })}`);
    return { ...data, cohorts };
}

let reader;
export function readAPI(data) {
    const phase = () => phaseAt(exec.scenario.startTime);
    try {
        // One session per reader VU, retained across its iterations.
        if (!reader) reader = requester(phase).api;
        const db = data.targets[exec.scenario.iterationInTest % data.targets.length];
        const base = `/api/v1/databases/${db.id}`;
        reader.request('GET', '/api/v1/databases', null, [200], 'database_list');
        reader.request('GET', `${base}/schemas`, null, [200], 'schema_list');
        reader.request('GET', `${base}/migrations?limit=20`, null, [200], 'migration_history');
        reader.request('GET', `${base}/jobs?limit=20`, null, [200], 'job_list');
    } catch (e) {
        metrics.errors.add(1, { phase: phase(), workload: 'api' });
        // Count failures without aborting other scenarios. Reauthenticate on expired session.
        if (String(e).includes('HTTP 401')) reader = undefined;
    }
    sleep(0.5);
}

const terminal = ['success', 'failed', 'partial', 'partially_reverted', 'resumed'];
function poll(api, fixture, id, seconds, tags) {
    const until = Date.now() + seconds * 1000;
    const path = `/api/v1/databases/${fixture.databaseId}/jobs/${id}`;
    do {
        try {
            const result = api.request('GET', path, null, [200], 'job_status', tags);
            if (terminal.includes(result.job.status)) return result;
            // 'unknown' may still be reconciled by the worker, so continue polling.
        } catch (e) {
            metrics.errors.add(1, { ...tags, workload: 'poll' });
            if (String(e).includes('HTTP 401') || String(e).includes('HTTP 403')) throw e;
        }
        sleep(0.5);
    } while (Date.now() < until);
    throw new Error(`Job ${id} did not reach a terminal status within ${seconds}s`);
}

export function submitJob(data) {
    const cohort = __ENV.STRESS_COHORT;
    const p = phases.find(x => x.name === cohort);
    const phase = () => phaseAt(exec.scenario.startTime - (p.offset + p.ramp) * 1000);
    const fixture = data.cohorts[cohort][exec.scenario.iterationInTest];
    if (!fixture) throw new Error('Missing unique review for iteration');
    const tags = { cohort, target: fixture.target };
    const currentTags = () => ({ ...tags, phase: phase() });
    let api, job;
    metrics.attempted.add(1, currentTags());
    try {
        api = requester(phase).api;
        job = enqueue(api, fixture, tags);
        metrics.accepted.add(1, currentTags());
        const result = poll(api, fixture, job.id, 120, tags);
        const done = result.job;
        const measuredTags = currentTags();
        metrics.terminal.add(1, measuredTags);
        (done.status === 'success' ? metrics.success : metrics.unsuccessful).add(1, measuredTags);
        metrics.retries.add(Math.max(0, done.attempt_count - 1), measuredTags);
        // For jobs with retry this is first start to final finish, including retry delays.
        if (done.started_at && done.finished_at) {
            const t = jobTiming(done);
            metrics.queue.add(t.queueWaitMs, measuredTags);
            metrics.duration.add(t.executionMs, measuredTags);
            metrics.endToEnd.add(t.endToEndMs, measuredTags);
        }
        console.log(`K6_JOB ${JSON.stringify({ cohort, phase: measuredTags.phase, target: fixture.target, review: fixture.reviewId, job: done.id, status: done.status, created_at: done.created_at, started_at: done.started_at, finished_at: done.finished_at, attempts: done.attempt_count, error_code: done.error_code })}`);
    } catch (e) {
        metrics.unresolved.add(1, currentTags());
        console.error(`K6_JOB_ERROR cohort=${cohort} target=${fixture.target} review=${fixture.reviewId} job=${job?.id || 'unknown'} ${e}`);
    } finally {
        if (api) {
            try { api.logout(); } catch (_) { metrics.errors.add(1, { ...currentTags(), workload: 'logout' }); }
        }
    }
}

// Independent audit of every prepared review, including ambiguous HTTP submissions.
// No audit requests enter measured phase thresholds or throughput.
export function teardown(data) {
    const { api } = requester('audit');
    const expected = new Map(data.reviews.map(r => [r.reviewId, r]));
    const found = new Map();
    try {
        for (const db of data.targets) {
            let before = null;
            for (let page = 0; page < 100; page++) {
                const result = api.request('GET', `/api/v1/databases/${db.id}/jobs?limit=100${before ? `&before=${before}` : ''}`, null, [200], 'audit_list');
                for (const j of result.jobs) if (expected.has(j.review_id)) found.set(j.review_id, j);
                if (!result.has_more || !result.next_before) break;
                before = result.next_before;
            }
        }
        // Resolve any remaining accepted jobs within a bounded shared audit window.
        const until = Date.now() + 90000;
        for (const [reviewId, fixture] of expected) {
            let j = found.get(reviewId);
            if (j && !terminal.includes(j.status) && Date.now() < until) {
                try { j = poll(api, fixture, j.id, Math.max(1, (until - Date.now()) / 1000), { target: fixture.target }).job; }
                catch (_) { /* keep unresolved status visible */ }
            }
            metrics.auditSuccess.add(j?.status === 'success' ? 1 : 0);
            console.log(`K6_AUDIT review=${reviewId} target=${fixture.target} job=${j?.id || 'none'} status=${j?.status || 'not_found'}`);
        }
    } finally { api.logout(); }
}
