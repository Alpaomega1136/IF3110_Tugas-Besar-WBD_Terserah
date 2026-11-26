//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/database"
)

func TestViewerAPI(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "viewer-target")
	target := createTarget(t, admin, input)
	other := createTarget(t, admin, targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "other-viewer-target"))
	db, err := database.Open(ctx, strings.TrimSpace(os.Getenv("TEST_TARGET_DATABASE_URL")))
	if err != nil {
		t.Fatal("cannot open viewer test target")
	}
	defer db.Close()
	schema := fmt.Sprintf("viewer_%d", time.Now().UnixNano())
	qSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = db.Exec(ctx, "CREATE SCHEMA "+qSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.Exec(ctx, "DROP SCHEMA "+qSchema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	name := `odd " table`
	qualified := pgx.Identifier{schema, name}.Sanitize()
	_, err = db.Exec(ctx, "CREATE TABLE "+qualified+" (id bigint PRIMARY KEY, amount numeric(30,4) CHECK(amount>=0), note text, optional text); CREATE INDEX note_idx ON "+qualified+"(note); INSERT INTO "+qualified+" VALUES (9007199254740993,12345678901234567890.1234,'hello',NULL),(9007199254740994,0,'',NULL); CREATE TABLE "+qSchema+".empty_table(id integer)")
	if err != nil {
		t.Fatal(err)
	}
	grant(t, a, "viewer", target.ID, "view")
	grant(t, a, "editor", target.ID, "edit")
	base := fmt.Sprintf("/api/v1/databases/%d/schemas", target.ID)
	structure := base + "/" + schema + "/structure"
	status(t, a.browser().request("GET", base, nil), 401)
	status(t, a.browser().request("GET", structure, nil), 401)
	for _, user := range []string{"viewer", "editor"} {
		b := a.browser()
		b.login(t, user)
		w := b.request("GET", base, nil)
		status(t, w, 200)
		if !strings.Contains(w.Body.String(), schema) {
			t.Fatal("schema missing")
		}
		path := base + "/" + schema + "/tables"
		status(t, b.request("GET", path, nil), 200)
		object := path + "/" + url.PathEscape(name)
		w = b.request("GET", object, nil)
		status(t, w, 200)
		var meta struct {
			Columns     []any `json:"columns"`
			Constraints []struct {
				Type string `json:"type"`
			} `json:"constraints"`
			Indexes []any `json:"indexes"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &meta); err != nil || len(meta.Columns) != 4 || len(meta.Indexes) != 2 {
			t.Fatalf("metadata incomplete: %s", w.Body.String())
		}
		constraintTypes := map[string]bool{}
		for _, constraint := range meta.Constraints {
			constraintTypes[constraint.Type] = true
		}
		if !constraintTypes["p"] || !constraintTypes["c"] {
			t.Fatal("primary key or check constraint missing")
		}
		w = b.request("GET", structure, nil)
		status(t, w, 200)
		var summary struct {
			Tables []struct {
				Name           string `json:"name"`
				Kind           string `json:"kind"`
				Columns        []any  `json:"columns"`
				HasConstraints bool   `json:"has_constraints"`
				HasIndexes     bool   `json:"has_indexes"`
			} `json:"tables"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &summary); err != nil || len(summary.Tables) != 2 {
			t.Fatalf("schema structure incomplete: %s", w.Body.String())
		}
		objectSummary := summary.Tables[1]
		if objectSummary.Name != name || objectSummary.Kind != "table" || len(objectSummary.Columns) != len(meta.Columns) || !objectSummary.HasConstraints || !objectSummary.HasIndexes {
			t.Fatalf("table missing columns, constraints or indexes: %+v", objectSummary)
		}
		w = b.request("GET", object+"/rows?limit=1", nil)
		status(t, w, 200)
		var rows struct {
			Rows     [][]any `json:"rows"`
			More     bool    `json:"has_more"`
			Ordering string  `json:"ordering"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows.Rows) != 1 || !rows.More || rows.Ordering != "primary_key" {
			t.Fatal("invalid pagination")
		}
		if rows.Rows[0][0] != "9007199254740993" || rows.Rows[0][1] != "12345678901234567890.1234" || rows.Rows[0][3] != nil {
			t.Fatal("lost precision or NULL")
		}
		w = b.request("GET", object+"/rows?limit=1&offset=1", nil)
		status(t, w, 200)
		if err = json.Unmarshal(w.Body.Bytes(), &rows); err != nil || rows.More || rows.Rows[0][2] != "" {
			t.Fatal("invalid second page")
		}
		w = b.request("GET", path+"/empty_table/rows", nil)
		status(t, w, 200)
		if !strings.Contains(w.Body.String(), `"rows":[]`) {
			t.Fatal("empty table not represented as array")
		}
		status(t, b.request("GET", object+"/rows?limit=201", nil), 400)
		status(t, b.request("GET", path+"/missing", nil), 404)
		status(t, b.request("GET", base+"/missing_schema/tables", nil), 404)
		status(t, b.request("GET", base+"/pg_catalog/tables/pg_authid/rows", nil), 403)
		status(t, b.request("GET", base+"/pg_catalog/structure", nil), 403)
		status(t, b.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", other.ID), nil), 403)
		status(t, b.request("GET", path+"/"+url.PathEscape(`x"; DROP SCHEMA public CASCADE; --`), nil), 404)
	}
	for _, user := range []string{"outsider", "admin"} {
		b := a.browser()
		b.login(t, user)
		status(t, b.request("GET", base, nil), 403)
		status(t, b.request("GET", structure, nil), 403)
	}
	offlineInput := input
	offlineInput.Name = "offline-viewer-target"
	offlineInput.Port = 1
	offline := createTarget(t, admin, offlineInput)
	grant(t, a, "viewer", offline.ID, "view")
	b := a.browser()
	b.login(t, "viewer")
	w := b.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", offline.ID), nil)
	status(t, w, 503)
	if strings.Contains(w.Body.String(), *input.Password) {
		t.Fatal("target connection error leaked credentials")
	}
	var count int
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+qualified).Scan(&count); err != nil || count != 2 {
		t.Fatal("viewer changed target data")
	}
	// Schema Diff must reject an oversized schema rather than comparing a partial result.
	var ddl strings.Builder
	for i := 0; i < 99; i++ {
		fmt.Fprintf(&ddl, "CREATE TABLE %s.extra_%d ();", qSchema, i)
	}
	if _, err = db.Exec(ctx, ddl.String()); err != nil {
		t.Fatal(err)
	}
	w = b.request("GET", structure, nil)
	status(t, w, 413)
	if !strings.Contains(w.Body.String(), "too_many_tables") {
		t.Fatalf("unexpected schema limit error: %s", w.Body.String())
	}
}
