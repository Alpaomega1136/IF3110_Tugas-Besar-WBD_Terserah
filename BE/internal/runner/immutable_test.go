package runner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImmutableHTTPError(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, errDraftLocked)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"migration_immutable"`) {
		t.Fatalf("perubahan definisi yang terkunci harus ditolak 409: %d %s", w.Code, w.Body.String())
	}
}
