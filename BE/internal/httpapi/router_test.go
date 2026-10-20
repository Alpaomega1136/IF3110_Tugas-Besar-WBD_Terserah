package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubDB struct{ err error }

func (s stubDB) Ping(context.Context) error { return s.err }

func TestHealth(t *testing.T) {
	for _, tc := range []struct {
		path   string
		err    error
		status int
	}{
		{"/health/live", errors.New("offline"), 200},
		{"/health/ready", nil, 200},
		{"/health/ready", errors.New("password=secret"), 503},
		{"/api/v1/not-implemented", nil, 404},
	} {
		rr := httptest.NewRecorder()
		New(stubDB{tc.err}).ServeHTTP(rr, httptest.NewRequest("GET", tc.path, nil))
		if rr.Code != tc.status {
			t.Fatalf("%s: got %d", tc.path, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "secret") {
			t.Fatal("driver error leaked")
		}
	}
}
