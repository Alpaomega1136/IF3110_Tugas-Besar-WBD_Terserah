import { BASE_URL, required, requester, approver } from './client.js';

function findTarget(databases, name) {
    const matches = databases.filter(db => db.database_name === name && db.access === 'edit');
    if (matches.length !== 1) throw new Error(`Expected exactly one accessible edit target for ${name}`);
    return matches[0];
}

// Create fresh approved reviews outside the measured workload. Never reuse review IDs.
export function prepareFixtures(countA, countB) {
    if (required('K6_CONFIRM_TEST_DB') !== 'YES') throw new Error('Confirm disposable test targets with K6_CONFIRM_TEST_DB=YES');
    if (!/^http:\/\/(localhost|127\.0\.0\.1)(:\d+)?$/.test(BASE_URL)) throw new Error('This fixture only supports a local HTTP test instance');
    if (![countA, countB].every(n => Number.isInteger(n) && n > 0 && n <= 500)) throw new Error('Expected 1..500 reviews per target');
    const nameA = __ENV.K6_TARGET_A || 'wbd_dev_target_a';
    const nameB = __ENV.K6_TARGET_B || 'wbd_dev_target_b';
    if (nameA === nameB) throw new Error('Two distinct target databases required');
    const req = requester('setup');
    const lead = approver('setup');
    const runId = `${Date.now().toString(36)}_${Math.floor(Math.random() * 0x1000000).toString(16)}`;
    try {
        if (req.user.id === lead.user.id) throw new Error('Use distinct requester and group leader accounts');
        const databases = req.api.request('GET', '/api/v1/databases', null, [200], 'database_list').databases;
        const a = findTarget(databases, nameA);
        const b = findTarget(databases, nameB);
        if (a.id === b.id) throw new Error('Targets must have distinct connection IDs');
        const targets = [a, b].map(db => ({ id: db.id, name: db.database_name }));
        const reviews = [];
        for (const [targetIndex, count] of [countA, countB].entries()) {
            const db = targets[targetIndex];
            const base = `/api/v1/databases/${db.id}`;
            // Verify actual target connectivity and requester read permissions.
            req.api.request('GET', `${base}/schemas`, null, [200], 'schema_list');
            for (let i = 0; i < count; i++) {
                const schema = `k6_${runId}_${targetIndex}_${i}`;
                // Identifiers are generated internally; no input or secret is interpolated into SQL.
                const draft = req.api.request('POST', `${base}/drafts`, {
                    title: `k6 ${runId} target ${targetIndex} job ${i}`,
                    sql: `CREATE SCHEMA ${schema}; CREATE TABLE ${schema}.items (id integer PRIMARY KEY); INSERT INTO ${schema}.items SELECT generate_series(1, 1000); SELECT pg_catalog.pg_sleep(0.5);`,
                    sql_down: `DROP SCHEMA ${schema} CASCADE;`,
                    irreversible: false,
                }, [201], 'draft_create');
                let review = req.api.request('POST', `${base}/reviews`, {
                    draft_id: String(draft.id), revision: String(draft.revision), action_type: 'up',
                }, [201], 'review_submit').review;
                if (review.status !== 'pending_review' || !review.approvals.length ||
                    review.approvals.some(a => a.leader_user_id !== lead.user.id)) {
                    throw new Error('Every accessing group must use the configured test leader; check fixture grants');
                }
                review = lead.api.request('POST', `${base}/reviews/${review.id}/approve`, {
                    note: 'Approved disposable k6 test migration',
                }, [200], 'review_approve').review;
                if (review.status !== 'approved') throw new Error(`Review ${review.id} not fully approved`);
                reviews.push({ databaseId: db.id, target: targetIndex === 0 ? 'a' : 'b', reviewId: review.id, draftId: draft.id, schema });
            }
        }
        console.log(`Prepared ${reviews.length} unique approved reviews (A=${countA}, B=${countB}), run=${runId}`);
        return { runId, targets, reviews };
    } finally {
        lead.api.logout();
        req.api.logout();
    }
}
