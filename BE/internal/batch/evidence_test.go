package batch

import (
	"encoding/json"
	"testing"
)

func TestEvidenceBindsRecoveryDataAndCheckpointChain(t *testing.T) {
	key := []byte("private-test-key")
	m := Manifest{ExecutionID: "e1", DatabaseID: 1, Version: 2, ActorID: 3, SQL: "UPDATE snapshot", TableOID: 4, ScopeCount: 2, ScopeHash: "scope"}
	manifest := ManifestSignature(key, m)
	changed := m
	changed.ScopeCount++
	if manifest == ManifestSignature(key, changed) {
		t.Fatal("scope not bound")
	}
	changed = m
	changed.SQL = "UPDATE altered"
	if manifest == ManifestSignature(key, changed) || manifest == ManifestSignature([]byte("other key"), m) {
		t.Fatal("snapshot or key not bound")
	}
	rows := []Row{{Key: 1, Before: json.RawMessage(`{"value":1}`), After: json.RawMessage(`{"value":2}`)}}
	hash, err := RowsHash(rows)
	if err != nil {
		t.Fatal(err)
	}
	c := Checkpoint{BatchNo: 1, LastKey: 1, RowCount: 1, CommittedRows: 1, RowHash: hash, PreviousSignature: manifest}
	sig := CheckpointSignature(key, manifest, c)
	rows[0].Before = json.RawMessage(`{"value":99}`)
	c.RowHash, err = RowsHash(rows)
	if err != nil {
		t.Fatal(err)
	}
	if sig == CheckpointSignature(key, manifest, c) {
		t.Fatal("before image not protected")
	}
	c.RowHash = hash
	c.PreviousSignature = "other"
	if sig == CheckpointSignature(key, manifest, c) {
		t.Fatal("checkpoint chain not protected")
	}
	rows[0].After = json.RawMessage("broken")
	if _, err = RowsHash(rows); err == nil {
		t.Fatal("malformed evidence accepted")
	}
}

func TestCheckpointBindsRetryIdentityAndPreservesLegacySignatures(t *testing.T) {
	c := Checkpoint{BatchNo: 1, LastKey: 2, RowCount: 2, CommittedRows: 2, RowHash: "rows", PreviousSignature: "root"}
	legacy := `{"BatchNo":1,"LastKey":2,"RowCount":2,"CommittedRows":2,"RowHash":"rows","PreviousSignature":"root","Signature":""}`
	encoded, err := json.Marshal(c)
	if err != nil || string(encoded) != legacy {
		t.Fatal("legacy signed payload changed", string(encoded), err)
	}
	key := []byte("test-key")
	c.AttemptExecutionID, c.AttemptVersion = "retry-1", 2
	sig := CheckpointSignature(key, "manifest", c)
	c.AttemptExecutionID = "retry-2"
	if sig == CheckpointSignature(key, "manifest", c) {
		t.Fatal("attempt ID not protected")
	}
	c.AttemptExecutionID, c.AttemptVersion = "retry-1", 3
	if sig == CheckpointSignature(key, "manifest", c) {
		t.Fatal("attempt version not protected")
	}
}
