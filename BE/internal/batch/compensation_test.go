package batch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompensationDefinitionBindsScopeColumnsAndQuery(t *testing.T) {
	c := Config{Schema: `demo"schema`, Table: "items", Key: "id", Size: 10, Set: map[string]json.RawMessage{"value": json.RawMessage("null")}}
	down, err := DownSQL(c)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDown(down)
	if err != nil || parsed == nil || parsed.Relation() != c.Relation() {
		t.Fatal("canonical definition did not round trip", err)
	}
	for _, fragment := range []string{`SET "value" = (saved.row_before)."value"`, `target."id" = (saved.row_before)."id"`, `pg_catalog.to_jsonb(target) = saved.row_after`, `$1::jsonb`} {
		if !strings.Contains(down, fragment) {
			t.Fatal("missing compensation boundary", fragment)
		}
	}
	for _, bad := range []string{strings.Replace(down, "saved.row_after", "'{}'::jsonb", 1), down + "; DELETE FROM items", strings.Replace(down, `"key":"id"`, `"key":"other"`, 1)} {
		if _, err := ParseDown(bad); err == nil {
			t.Fatal("modified Down definition accepted")
		}
	}
	if sql, err := DownSQL(Config{}); err == nil || sql != "" {
		t.Fatal("invalid configuration accepted")
	}
}

func TestCompensationEvidenceBindsOriginalExecutionAndImmutableDown(t *testing.T) {
	m := CompensationManifest{ExecutionID: "down", SourceExecutionID: "up", SourceSignature: "original-chain", SQL: "approved down", DatabaseID: 1, Version: 2, ActorID: 3, Batches: 5, Rows: 50}
	key := []byte("test signing key")
	signature := CompensationSignature(key, m)
	for _, change := range []func(*CompensationManifest){func(m *CompensationManifest) { m.SourceExecutionID = "other" }, func(m *CompensationManifest) { m.SQL = "other" }, func(m *CompensationManifest) { m.Batches++ }} {
		other := m
		change(&other)
		if CompensationSignature(key, other) == signature {
			t.Fatal("compensation proof did not bind its immutable coordinates")
		}
	}
}
