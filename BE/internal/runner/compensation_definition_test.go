package runner

import (
	"encoding/json"
	"terserah/backend/internal/batch"
	"testing"
)

func TestBatchDownMustMatchImmutableUpConfiguration(t *testing.T) {
	c := batch.Config{Schema: "public", Table: "items", Key: "id", Size: 2, Set: map[string]json.RawMessage{"value": json.RawMessage("2")}}
	up, _ := batch.Encode(c)
	down, _ := batch.DownSQL(c)
	if _, err := validasiDefinisiDraft(draftInput{Title: "restore", SQL: up, SQLDown: &down}); err != nil {
		t.Fatal(err)
	}
	c.Size = 3
	other, _ := batch.DownSQL(c)
	for _, bad := range []string{other, "UPDATE public.items SET value=0"} {
		if _, err := validasiDefinisiDraft(draftInput{Title: "unsafe", SQL: up, SQLDown: &bad}); err == nil {
			t.Fatal("unbound Down was accepted")
		}
	}
	if _, err := validasiDefinisiDraft(draftInput{Title: "wrong mode", SQL: "SELECT 1", SQLDown: &down}); err == nil {
		t.Fatal("batch Down attached to normal Up")
	}
	if err := validateDraftSQL(down); err == nil {
		t.Fatal("batch Down accepted as standalone Up")
	}
}
