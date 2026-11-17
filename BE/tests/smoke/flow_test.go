//go:build integration

package smoke_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/database"
	"terserah/backend/internal/runner"
)

func TestProductionHTTPFlow(t *testing.T) {
	origin, password := startAPI(t, "bootstrap")
	admin := newBrowser(t, origin, password)
	admin.call(t, "GET", "/health/live", nil, 200, nil)
	admin.login(t, "demo-admin")
	admin.call(t, "GET", "/api/v1/auth/me", nil, 200, nil)
	users := map[string]string{}
	for _, name := range []string{"demo-editor", "demo-viewer", "demo-outsider"} {
		var out struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		admin.call(t, "POST", "/api/v1/admin/users", map[string]string{"username": name, "password": password}, 201, &out)
		if out.User.ID == "" {
			t.Fatal("user ID missing")
		}
		users[name] = out.User.ID
	}
	var targets []connections.Connection
	for i, variable := range []string{"TEST_TARGET_DATABASE_URL", "TEST_SECOND_TARGET_DATABASE_URL"} {
		value := os.Getenv(variable)
		if value == "" {
			t.Fatalf("%s must name a provisioned restricted fixture target", variable)
		}
		cfg, err := pgx.ParseConfig(value)
		if err != nil {
			t.Fatal("invalid target fixture URL")
		}
		if cfg.TLSConfig != nil {
			t.Fatal("local HTTP smoke fixtures require explicit sslmode=disable")
		}
		var target connections.Connection
		admin.call(t, "POST", "/api/v1/admin/databases", connections.Input{Name: fmt.Sprintf("demo-target-%d", i+1), Host: cfg.Host, Port: int(cfg.Port), DatabaseName: cfg.Database, Username: cfg.User, Password: &cfg.Password, SSLMode: "disable"}, 201, &target)
		admin.call(t, "POST", fmt.Sprintf("/api/v1/admin/databases/%d/test", target.ID), nil, 200, nil)
		targets = append(targets, target)
	}
	if targets[0].DatabaseName == targets[1].DatabaseName {
		t.Fatal("two distinct target fixture databases required")
	}
	base := fmt.Sprintf("/api/v1/databases/%d", targets[0].ID)
	var editGroup string
	for _, entry := range []struct{ name, access string }{{"demo-editor", "edit"}, {"demo-viewer", "view"}} {
		var out struct {
			Group struct {
				ID string `json:"id"`
			} `json:"group"`
		}
		admin.call(t, "POST", "/api/v1/admin/groups", map[string]string{"name": entry.name}, 201, &out)
		path := "/api/v1/admin/groups/" + out.Group.ID
		admin.call(t, "PUT", path+"/users/"+users[entry.name], nil, 200, nil)
		admin.call(t, "PUT", path+"/leader", map[string]string{"user_id": users[entry.name]}, 200, nil)
		admin.call(t, "PUT", fmt.Sprintf("%s/databases/%d", path, targets[0].ID), map[string]string{"access_level": entry.access}, 200, nil)
		if entry.access == "edit" {
			editGroup = path
		}
	}
	editor := newBrowser(t, origin, password)
	editor.login(t, "demo-editor")
	viewer := newBrowser(t, origin, password)
	viewer.login(t, "demo-viewer")
	outsider := newBrowser(t, origin, password)
	outsider.login(t, "demo-outsider")
	newBrowser(t, origin, password).call(t, "GET", base+"/schemas", nil, 401, nil)
	admin.call(t, "GET", base+"/schemas", nil, 403, nil)
	outsider.call(t, "GET", base+"/schemas", nil, 403, nil)
	editor.call(t, "GET", fmt.Sprintf("/api/v1/databases/%d/schemas", targets[1].ID), nil, 403, nil)
	for _, entry := range []struct {
		b      *browser
		access string
	}{{editor, "edit"}, {viewer, "view"}, {outsider, ""}} {
		var out struct {
			Databases []connections.Connection `json:"databases"`
		}
		entry.b.call(t, "GET", "/api/v1/databases", nil, 200, &out)
		if entry.access == "" {
			if len(out.Databases) != 0 {
				t.Fatal("outsider sees targets")
			}
		} else if len(out.Databases) != 1 || string(out.Databases[0].Access) != entry.access {
			t.Fatal("accessible target or access contract mismatch")
		}
	}
	db, err := database.Open(context.Background(), os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal("cannot open smoke target")
	}
	defer db.Close()
	schema := fmt.Sprintf("demo_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.Exec(ctx, "DROP SCHEMA IF EXISTS "+q+" CASCADE"); err != nil {
			t.Error("cannot clean demo target schema")
		}
	}()
	sql := demoSQL(t, "001_create.sql", schema)
	var draft runner.Draft
	editor.call(t, "POST", base+"/drafts", map[string]string{"title": "Demo create", "sql": sql}, 201, &draft)
	path := fmt.Sprintf("%s/drafts/%d", base, draft.ID)
	viewer.call(t, "POST", path+"/execute", map[string]string{"revision": "1"}, 403, nil)
	csrf := editor.csrf
	editor.csrf = "invalid"
	editor.call(t, "POST", path+"/execute", map[string]string{"revision": "1"}, 403, nil)
	editor.csrf = csrf
	approve := func(revision string) {
		t.Helper()
		var review struct {
			Review struct {
				ID string `json:"id"`
			} `json:"review"`
		}
		editor.call(t, "POST", base+"/reviews", map[string]string{"draft_id": fmt.Sprint(draft.ID), "revision": revision}, 201, &review)
		for _, leader := range []*browser{editor, viewer} {
			leader.call(t, "POST", base+"/reviews/"+review.Review.ID+"/approve", map[string]string{"note": "approved for test"}, 200, nil)
		}
	}
	approve("1")
	var success runner.Result
	editor.call(t, "POST", path+"/execute", map[string]string{"revision": "1"}, 200, &success)
	if success.Status != "success" || success.Version != 1 {
		t.Fatal("migration did not commit")
	}
	var replay runner.Result
	editor.call(t, "POST", path+"/execute", map[string]string{"revision": "1"}, 200, &replay)
	if replay.ExecutionID != success.ExecutionID || replay.Status != "success" {
		t.Fatal("execute replay changed attempt")
	}
	viewer.call(t, "GET", base+"/schemas", nil, 200, nil)
	object := base + "/schemas/" + schema + "/tables/items"
	viewer.call(t, "GET", base+"/schemas/"+schema+"/tables", nil, 200, nil)
	viewer.call(t, "GET", object, nil, 200, nil)
	var rows struct {
		Rows [][]any `json:"rows"`
		More bool    `json:"has_more"`
	}
	viewer.call(t, "GET", object+"/rows?limit=1", nil, 200, &rows)
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 3 || rows.Rows[0][0] != "9007199254740993" || rows.Rows[0][1] != "123.4500" || !rows.More {
		t.Fatal("viewer precision or pagination contract changed")
	}
	viewer.call(t, "GET", object+"/rows?limit=1&offset=1", nil, 200, &rows)
	if len(rows.Rows) != 1 || rows.Rows[0][2] != nil || rows.More {
		t.Fatal("viewer NULL contract changed")
	}
	badSQL := demoSQL(t, "002_rollback.sql", schema)
	editor.call(t, "PATCH", path, map[string]string{"title": "Demo rollback", "sql": badSQL, "revision": "1"}, 200, &draft)
	editor.call(t, "POST", path+"/execute", map[string]string{"revision": "1"}, 409, nil)
	approve("2")
	var failed runner.Result
	editor.call(t, "POST", path+"/execute", map[string]string{"revision": "2"}, 200, &failed)
	if failed.Status != "failed" || failed.Version != 2 || failed.ErrorCode == nil || *failed.ErrorCode != "22012" {
		t.Fatal("migration failure contract changed")
	}
	viewer.call(t, "GET", object+"/rows?limit=10", nil, 200, &rows)
	if len(rows.Rows) != 2 {
		t.Fatal("failed migration retained DML")
	}
	viewer.call(t, "GET", base+"/schemas/"+schema+"/tables/rollback_proof", nil, 404, nil)
	var detail runner.History
	viewer.call(t, "GET", base+"/migrations/1", nil, 200, &detail)
	if detail.SQL != sql || detail.Status != "success" || detail.FinishedAt == nil {
		t.Fatal("committed history snapshot changed")
	}
	viewer.call(t, "GET", base+"/migrations/2", nil, 200, &detail)
	if detail.SQL != badSQL || detail.Status != "failed" {
		t.Fatal("failed history snapshot missing")
	}
	var history struct {
		Items []runner.History `json:"migrations"`
		More  bool             `json:"has_more"`
		Next  *string          `json:"next_before"`
	}
	viewer.call(t, "GET", base+"/migrations?limit=1", nil, 200, &history)
	if len(history.Items) != 1 || history.Items[0].Version != 2 || !history.More || history.Next == nil || *history.Next != "2" {
		t.Fatal("history pagination changed")
	}
	viewer.call(t, "POST", base+"/migrations/reconcile", nil, 403, nil)
	editor.call(t, "POST", base+"/migrations/reconcile", nil, 200, nil)
	admin.call(t, "DELETE", fmt.Sprintf("%s/databases/%d", editGroup, targets[0].ID), nil, 200, nil)
	editor.call(t, "GET", "/api/v1/auth/me", nil, 401, nil)
	editor.login(t, "demo-editor")
	editor.call(t, "GET", base+"/migrations", nil, 403, nil)
	viewer.call(t, "POST", "/api/v1/auth/logout", nil, 204, nil)
	viewer.call(t, "GET", "/api/v1/auth/me", nil, 401, nil)
}

func demoSQL(t *testing.T, name, schema string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "demo", name))
	if err != nil {
		t.Fatal("cannot read versioned demo SQL")
	}
	return strings.ReplaceAll(string(data), "wbd_demo", schema)
}

func TestLegacySeedCLI(t *testing.T) {
	origin, password := startAPI(t, "seed")
	admin := newBrowser(t, origin, password)
	admin.login(t, "demo-admin")
	admin.call(t, "GET", "/api/v1/admin/users", nil, 200, nil)
}
