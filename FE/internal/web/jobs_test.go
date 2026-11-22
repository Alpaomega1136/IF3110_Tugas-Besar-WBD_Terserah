package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobMonitoringRoutesAndAccessibleShell(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	for _, path := range []string{"/databases/9007199254740993/jobs", "/databases/9007199254740993/jobs/9007199254741001"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("job route failed", path, w.Code)
		}
		for _, value := range []string{`data-database-id="9007199254740993"`, `id="jobs-workspace" hidden`, `id="jobs-attempts-body"`, `id="jobs-request-resume"`, `aria-atomic="true"`, `id="global-confirm-dialog"`, `/static/js/jobs.js`, `role="region" aria-labelledby="jobs-attempts-heading" tabindex="0"`, `role="region" aria-labelledby="jobs-list-heading" tabindex="0"`} {
			if !strings.Contains(w.Body.String(), value) {
				t.Errorf("%s missing %s", path, value)
			}
		}
	}
	for _, path := range []string{"/databases/0/jobs", "/databases/not-id/jobs/1", "/databases/1/jobs/0", "/databases/1/jobs/9223372036854775808"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid job route accepted", path, w.Code)
		}
	}
}
