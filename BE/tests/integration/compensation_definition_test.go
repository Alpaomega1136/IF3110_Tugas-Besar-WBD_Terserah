//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
)

func TestCompensationSQLRestoresVerifiedImagesAndRejectsChangedRows(t *testing.T) {
	db, schema := batchTarget(t)
	ctx := context.Background()
	var protected bool
	if err := db.QueryRow(ctx, `SELECT bool_and(pg_catalog.has_table_privilege(current_user,name,'SELECT, INSERT')
 AND NOT pg_catalog.has_table_privilege(current_user,name,'UPDATE, DELETE, TRUNCATE'))
 FROM unnest(ARRAY['_terserah.compensation_runs','_terserah.compensation_checkpoints']) AS name`).Scan(&protected); err != nil || !protected {
		t.Fatal("compensation evidence is not append-only for the executor", err)
	}
	c := batch.Config{Schema: schema, Table: "items", Key: "id", Size: 2, Set: map[string]json.RawMessage{"amount": json.RawMessage("9007199254740993.125"), "note": json.RawMessage(`"after"`)}}
	if _, err := db.Exec(ctx, `CREATE TABLE `+c.Relation()+`(id bigint PRIMARY KEY,amount numeric(20,3),note text); INSERT INTO `+c.Relation()+` VALUES(1,NULL,'first'),(2,42.125,NULL),(3,7,'outside')`); err != nil {
		t.Fatal(err)
	}
	type image struct{ Before, After json.RawMessage }
	images := make([]image, 2)
	for i := range images {
		if err := db.QueryRow(ctx, `SELECT to_jsonb(t) FROM `+c.Relation()+` t WHERE id=$1`, i+1).Scan(&images[i].Before); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(ctx, c.Query(), []int64{1, 2}, c.Values())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key int64
		var data json.RawMessage
		if err := rows.Scan(&key, &data); err != nil {
			t.Fatal(err)
		}
		images[key-1].After = data
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	payload := []map[string]json.RawMessage{}
	for _, r := range images {
		payload = append(payload, map[string]json.RawMessage{"before": r.Before, "after": r.After})
	}
	data, _ := json.Marshal(payload)
	down, _ := batch.DownSQL(c)
	if _, err = db.Exec(ctx, `UPDATE `+c.Relation()+` SET note='external change' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	count := func(tx pgx.Tx) int {
		t.Helper()
		r, err := tx.Query(ctx, down, data)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		n := 0
		for r.Next() {
			n++
		}
		if r.Err() != nil {
			t.Fatal(r.Err())
		}
		return n
	}
	if n := count(tx); n != 1 {
		t.Fatalf("changed row must fail the image comparison, updated=%d", n)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var unchanged bool
	if err = db.QueryRow(ctx, `SELECT to_jsonb(t)=$1::jsonb FROM `+c.Relation()+` t WHERE id=1`, images[0].After).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("failed batch was not atomic", err)
	}
	if _, err = db.Exec(ctx, `UPDATE `+c.Relation()+` SET note='after' WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if n := count(tx); n != 2 {
		t.Fatal("verified images did not restore both rows", n)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i, r := range images {
		if err = db.QueryRow(ctx, `SELECT to_jsonb(t)=$1::jsonb FROM `+c.Relation()+` t WHERE id=$2`, r.Before, i+1).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("NULL or exact numeric value was not restored", err)
		}
	}
	if err = db.QueryRow(ctx, `SELECT amount=7 AND note='outside' FROM `+c.Relation()+` WHERE id=3`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("row outside approved scope changed", err)
	}
}
