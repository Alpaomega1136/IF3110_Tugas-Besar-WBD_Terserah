package runner

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stringPtr(value string) *string { return &value }

func TestValidasiDefinisiDraft(t *testing.T) {
	base := draftInput{Title: "  Tambah kolom  ", SQL: "ALTER TABLE users ADD COLUMN age int", SQLDown: stringPtr("ALTER TABLE users DROP COLUMN age")}
	tests := []struct {
		name  string
		input draftInput
		valid bool
	}{
		{"reversible", base, true},
		{"reversible SQL multi-perintah", draftInput{Title: base.Title, SQL: "ALTER TABLE users ADD COLUMN age int; UPDATE users SET age=18", SQLDown: stringPtr("UPDATE users SET age=NULL; ALTER TABLE users DROP COLUMN age")}, true},
		{"irreversible", draftInput{Title: "Hapus data", SQL: "DELETE FROM users", Irreversible: true, IrreversibleReason: stringPtr("  Data yang dihapus tidak dapat dipulihkan  ")}, true},
		{"tanpa judul", draftInput{SQL: base.SQL, SQLDown: base.SQLDown}, false},
		{"judul terlalu panjang", draftInput{Title: strings.Repeat("a", 201), SQL: base.SQL, SQLDown: base.SQLDown}, false},
		{"SQL Up kosong", draftInput{Title: base.Title, SQLDown: base.SQLDown}, false},
		{"SQL Up tidak didukung", draftInput{Title: base.Title, SQL: "COMMIT", SQLDown: base.SQLDown}, false},
		{"reversible tanpa SQL Down", draftInput{Title: base.Title, SQL: base.SQL}, false},
		{"reversible SQL Down kosong", draftInput{Title: base.Title, SQL: base.SQL, SQLDown: stringPtr("  ")}, false},
		{"reversible SQL Down terlarang", draftInput{Title: base.Title, SQL: base.SQL, SQLDown: stringPtr("ROLLBACK")}, false},
		{"reversible dengan alasan", draftInput{Title: base.Title, SQL: base.SQL, SQLDown: base.SQLDown, IrreversibleReason: stringPtr("Tidak perlu")}, false},
		{"irreversible tanpa alasan", draftInput{Title: base.Title, SQL: base.SQL, Irreversible: true}, false},
		{"irreversible alasan kosong", draftInput{Title: base.Title, SQL: base.SQL, Irreversible: true, IrreversibleReason: stringPtr("   ")}, false},
		{"irreversible dengan SQL Down", draftInput{Title: base.Title, SQL: base.SQL, SQLDown: base.SQLDown, Irreversible: true, IrreversibleReason: stringPtr("Tidak dapat dibatalkan")}, false},
		{"irreversible alasan terlalu panjang", draftInput{Title: base.Title, SQL: base.SQL, Irreversible: true, IrreversibleReason: stringPtr(strings.Repeat("a", 501))}, false},
		{"irreversible alasan NUL", draftInput{Title: base.Title, SQL: base.SQL, Irreversible: true, IrreversibleReason: stringPtr("hapus\x00data")}, false},
		{"SQL Down terlalu panjang", draftInput{Title: base.Title, SQL: base.SQL, SQLDown: stringPtr(strings.Repeat(" ", 48*1024+1))}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validasiDefinisiDraft(tc.input)
			if tc.valid && err != nil || !tc.valid && !errors.Is(err, errDraftDefinition) {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if tc.name == "reversible" && got.Title != "Tambah kolom" {
				t.Fatalf("judul tidak dinormalisasi: %q", got.Title)
			}
			if tc.name == "irreversible" && (got.IrreversibleReason == nil || *got.IrreversibleReason != "Data yang dihapus tidak dapat dipulihkan") {
				t.Fatalf("alasan tidak dinormalisasi: %v", got.IrreversibleReason)
			}
		})
	}
}

func TestDraftDefinitionJSON(t *testing.T) {
	var in draftInput
	if err := json.Unmarshal([]byte(`{"title":"Hapus kolom","sql":"ALTER TABLE users DROP COLUMN age","irreversible":true,"irreversible_reason":"Data di kolom hilang"}`), &in); err != nil {
		t.Fatal(err)
	}
	if _, err := validasiDefinisiDraft(in); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(Draft{ID: 5, Revision: 2, SQL: in.SQL, SQLDown: in.SQLDown, Irreversible: in.Irreversible, IrreversibleReason: in.IrreversibleReason})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["sql"] != in.SQL || fields["sql_down"] != nil || fields["irreversible"] != true || fields["irreversible_reason"] != "Data di kolom hilang" || fields["id"] != "5" || fields["revision"] != "2" {
		t.Fatalf("respons JSON tidak sesuai kontrak: %s", out)
	}
}

func TestDraftDefinitionHTTPError(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, errDraftDefinition)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_migration_definition"`) {
		t.Fatalf("respons harus 400 dengan kode yang tepat: %d %s", w.Code, w.Body.String())
	}
}
