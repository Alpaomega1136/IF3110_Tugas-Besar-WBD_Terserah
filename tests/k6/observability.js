import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { BASE_URL, required, requester, approver } from './lib/client.js';
import { enqueue, waitForJob } from './lib/jobs.js';

const verified = new Counter('obs_cases_verified');
export const options = {
    vus: 1, iterations: 1,
    thresholds: {
        checks: ['rate==1'],
        http_req_failed: ['rate==0'],
        obs_cases_verified: ['count==4'],
    },
};

const names = {
    success: 'terserah_migration_jobs{status="success"}',
    failed: 'terserah_migration_jobs{status="failed"}',
    queued: 'terserah_migration_jobs{status="queued"}',
    running: 'terserah_migration_jobs{status="running"}',
    recovering: 'terserah_migration_jobs{status="recovering"}',
    retryWait: 'terserah_migration_jobs{status="retry_wait"}',
    retries: 'terserah_migration_retry_attempts',
    initialCount: 'terserah_migration_queue_wait_seconds_count{attempt="initial"}',
    retryCount: 'terserah_migration_queue_wait_seconds_count{attempt="retry"}',
    successCount: 'terserah_migration_duration_seconds_count{outcome="success"}',
    failedCount: 'terserah_migration_duration_seconds_count{outcome="failed"}',
    batches: 'terserah_migration_batches_total{kind="migration"}',
    rows: 'terserah_migration_rows_total{kind="migration"}',
};
function verify(condition, message) {
    if (!check(condition, { [message]: value => value === true })) throw new Error(message);
}
function snapshot() {
    const response = http.get('http://127.0.0.1:9464/metrics', { timeout: '5s', tags: { name: 'metrics_snapshot' } });
    verify(response.status === 200, 'Metrics endpoint responds HTTP 200');
    const samples = {};
    for (const line of response.body.split(/\r?\n/)) {
        if (!line || line.startsWith('#')) continue;
        const split = line.indexOf(' ');
        if (split > 0) samples[line.slice(0, split)] = Number(line.slice(split + 1).trim());
    }
    const result = {};
    for (const [key, name] of Object.entries(names)) {
        verify(Number.isFinite(samples[name]), `Metric available: ${key}`);
        result[key] = samples[name];
    }
    return result;
}
function ensureIdle(m) {
    verify(['queued', 'running', 'recovering', 'retryWait'].every(k => m[k] === 0), 'No other queued, running or retrying jobs');
}
function execute(req, lead, databaseId, label, definition) {
    const base = `/api/v1/databases/${databaseId}`;
    const draft = req.api.request('POST', `${base}/drafts`, definition, [201], 'obs_draft_create');
    let review = req.api.request('POST', `${base}/reviews`, {
        draft_id: String(draft.id), revision: String(draft.revision), action_type: 'up',
    }, [201], 'obs_review_submit').review;
    verify(review.status === 'pending_review' && review.approvals.length > 0 &&
        review.approvals.every(a => a.leader_user_id === lead.user.id), 'Test leader can approve all required reviews');
    review = lead.api.request('POST', `${base}/reviews/${review.id}/approve`, {
        note: 'Approved isolated observability test fixture',
    }, [200], 'obs_review_approve').review;
    verify(review.status === 'approved', 'Fixture review approved');
    const fixture = { databaseId, reviewId: review.id };
    const job = enqueue(req.api, fixture);
    const result = waitForJob(req.api, fixture, job.id, 120);
    const j = result.job;
    console.log(`OBS_JOB case=${label} review=${review.id} job=${j.id} status=${j.status} code=${j.error_code || 'none'} attempts=${j.attempt_count} max_attempts=${j.max_attempts} batches=${j.committed_batches} rows=${j.committed_rows}`);
    for (const attempt of result.attempts) {
        console.log(`OBS_ATTEMPT case=${label} job=${j.id} attempt=${attempt.attempt_no} status=${attempt.status} code=${attempt.error_code || 'none'} execution_id=${attempt.execution_id}`);
    }
    return result;
}
function expected(result, status, code, count) {
    const j = result.job;
    verify(j.status === status && (j.error_code || '') === code && j.attempt_count === count && result.attempts.length === count,
        `Expected job outcome: ${status}, ${code || 'no error'}, ${count} attempt(s)`);
    verify(result.attempts.every(a => a.status === status && (a.error_code || '') === code), 'Attempt history matches expected outcome');
    verified.add(1);
}

export default function () {
    if (required('K6_CONFIRM_TEST_DB') !== 'YES' || required('K6_CONFIRM_OBS_FAILURES') !== 'YES') {
        throw new Error('Confirm disposable database and intentional failed jobs with both confirmation variables');
    }
    if (!/^http:\/\/(localhost|127\.0\.0\.1)(:\d+)?$/.test(BASE_URL)) throw new Error('Local test instance only');
    const name = required('K6_TARGET_A');
    if (name !== 'wbd_dev_target_a') throw new Error('Observability fixture requires the dedicated wbd_dev_target_a database');
    const before = snapshot();
    ensureIdle(before);
    const req = requester('obs');
    const lead = approver('obs');
    const runId = `${Date.now().toString(36)}_${Math.floor(Math.random() * 0x1000000).toString(16)}`;
    const schema = `obs_${runId}`;
    console.log(`OBS_RUN id=${runId} schema=${schema} target=${name}`);
    try {
        verify(req.user.id !== lead.user.id, 'Distinct requester and approver');
        const databases = req.api.request('GET', '/api/v1/databases', null, [200], 'database_list').databases;
        const matches = databases.filter(d => d.database_name === name && d.access === 'edit');
        verify(matches.length === 1, 'Exactly one editable disposable test target');
        const db = matches[0].id;
        // Create a fresh, isolated schema through the ordinary approved migration workflow.
        const setup = execute(req, lead, db, 'setup', {
            title: `obs ${runId} setup`,
            sql: `CREATE SCHEMA ${schema}; CREATE TABLE ${schema}.items (id bigint PRIMARY KEY, value integer NOT NULL); INSERT INTO ${schema}.items SELECT n, 0 FROM generate_series(1, 1000) n; CREATE FUNCTION ${schema}.retry_probe() RETURNS integer LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='40001', MESSAGE='observability retry fixture'; END; $$;`,
            sql_down: `DROP SCHEMA ${schema} CASCADE;`, irreversible: false,
        });
        expected(setup, 'success', '', 1);
        // The API encodes the canonical batch configuration. No manually crafted SQL header.
        const batch = execute(req, lead, db, 'batch', {
            title: `obs ${runId} batch`,
            batch: { schema, table: 'items', key: 'id', size: 100, set: { value: 1 } },
            irreversible: true, irreversible_reason: 'Disposable observability fixture; no reverse operation tested',
        });
        verify(batch.job.execution_mode === 'batch' && Number(batch.job.committed_batches) === 10 && Number(batch.job.committed_rows) === 1000,
            'Batch commits exactly 10 batches and 1000 rows');
        expected(batch, 'success', '', 1);
        // No writes to existing data: these statements fail inside their own transactions.
        const permanent = execute(req, lead, db, 'permanent_failure', {
            title: `obs ${runId} permanent failure`, sql: 'SELECT 1/0', sql_down: 'SELECT 1', irreversible: false,
        });
        expected(permanent, 'failed', '22012', 1);
        const transient = execute(req, lead, db, 'transient_failure', {
            title: `obs ${runId} transient failure`, sql: `SELECT ${schema}.retry_probe()`, sql_down: 'SELECT 1', irreversible: false,
        });
        verify(transient.job.max_attempts === 3, 'Default retry limit is three attempts');
        expected(transient, 'failed', '40001', 3);
        const after = snapshot();
        ensureIdle(after);
        const deltas = { success: 2, failed: 2, retries: 2, initialCount: 4, retryCount: 2, successCount: 2, failedCount: 4, batches: 10, rows: 1000 };
        for (const [key, expectedDelta] of Object.entries(deltas)) {
            const actual = after[key] - before[key];
            console.log(`OBS_METRIC name=${names[key]} before=${before[key]} after=${after[key]} delta=${actual} expected=${expectedDelta}`);
            verify(actual === expectedDelta, `Metric delta: ${key}`);
        }
        console.log(`OBS_RESULT run=${runId} verified=4 metrics=PASS`);
    } finally {
        lead.api.logout();
        req.api.logout();
    }
}
