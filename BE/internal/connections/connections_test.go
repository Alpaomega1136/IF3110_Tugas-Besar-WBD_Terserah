package connections

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	valid := Input{Name: " Demo ", Host: "LOCALHOST.", DatabaseName: "demo", Username: "demo"}
	got, err := normalize(valid)
	if err != nil || got.Host != "localhost" || got.Port != 5432 || got.SSLMode != "verify-full" || got.Name != "Demo" {
		t.Fatalf("normalization failed: %v", err)
	}
	for _, edit := range []func(*Input){
		func(i *Input) { i.Host = "localhost?sslmode=disable" },
		func(i *Input) { i.Host = "/tmp" },
		func(i *Input) { i.Port = -1 },
		func(i *Input) { i.Port = 65536 },
		func(i *Input) { i.DatabaseName = "" },
		func(i *Input) { i.Username = "x\x00" },
		func(i *Input) { i.SSLMode = "prefer" },
	} {
		in := valid
		edit(&in)
		if _, err = normalize(in); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestErrorDoesNotExposeCredentials(t *testing.T) {
	rr := httptest.NewRecorder()
	respondError(rr, errors.New("postgres://user:private-password@localhost/private"))
	if rr.Code != 503 || strings.Contains(rr.Body.String(), "private") {
		t.Fatal("driver credentials leaked")
	}
}
