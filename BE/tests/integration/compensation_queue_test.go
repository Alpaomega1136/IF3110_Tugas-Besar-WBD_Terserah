//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
	"testing"
	"time"
)

func TestCompensationQueueHandoffIsDurableAndFenced(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	config := batchConfig(schema)
	if _, err := db.Exec(ctx, `CREATE TABLE `+config.Relation()+`(id bigint PRIMARY KEY,value int); INSERT INTO `+config.Relation()+` VALUES(1,0)`); err != nil {
		t.Fatal(err)
	}
	down, _ := batch.DownSQL(config)
	response := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", f.first), map[string]any{"title": "Queue compensation contract", "batch": config, "sql_down": down})
	status(t, response, 201)
	var draft runner.Draft
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, f.a, f.editor, f.first, draft.ID, 1)
	source, _, err := f.service.EnqueueDraft(ctx, f.actor, f.first, draft.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	original := assertJob(t, f, source, jobs.Success, "")
	setujuiAksiMigration(t, f.a, f.editor, f.first, draft.ID, 1, "compensate", original.ExecutionID, down)
	job, replayed, err := f.service.EnqueueOperation(ctx, f.actor, f.first, "compensate", original.ExecutionID)
	if err != nil || replayed || job.Status != jobs.RetryWait || job.ParentJobID == nil || *job.ParentJobID != source.ID {
		t.Fatal("handoff was not atomic", job, err)
	}
	parent, _, err := f.service.Get(ctx, f.actor, f.first, source.ID)
	if err != nil || parent.Status != jobs.Resumed || parent.ResumedByJobID == nil || *parent.ResumedByJobID != job.ID {
		t.Fatal("source did not point to its recovery job", parent, err)
	}
	replay, again, err := jobs.New(f.a.db).EnqueueOperation(ctx, f.actor, f.first, "compensate", original.ExecutionID)
	if err != nil || !again || replay.ID != job.ID {
		t.Fatal("durable replay created a second compensation job", err)
	}
	claim, err := f.service.Claim(ctx, "compensation-queue-contract", time.Minute)
	if err != nil || claim == nil || claim.SQL != down || claim.RootExecutionID != original.ExecutionID || claim.CompensationRoot != claim.ExecutionID {
		t.Fatal("claim lost its immutable recovery coordinates", err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET compensation_root_execution_id=$2 WHERE id=$1`, job.ID, original.ExecutionID); err == nil {
		t.Fatal("compensation root could be replaced")
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Heartbeat(ctx, claim, time.Minute); err != jobs.ErrLeaseLost {
		t.Fatal("expired worker retained its lease", err)
	}
	var unchanged bool
	if err = db.QueryRow(ctx, `SELECT value=2 FROM `+config.Relation()+` WHERE id=1`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("queue/claim executed Down SQL", err)
	}
}
