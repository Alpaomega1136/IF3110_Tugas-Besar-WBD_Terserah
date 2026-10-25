package viewer

import (
	"net/http/httptest"
	"testing"
)

func TestPagination(t *testing.T) {
	for _, tc := range []struct {
		query string
		valid bool
	}{{"", true}, {"?limit=200&offset=3", true}, {"?limit=0", false}, {"?limit=201", false}, {"?limit=x", false}, {"?offset=-1", false}, {"?offset=9223372036854775808", false}} {
		_, _, err := pagination(httptest.NewRequest("GET", "/"+tc.query, nil))
		if (err == nil) != tc.valid {
			t.Fatalf("incorrect validation: %s", tc.query)
		}
	}
	if !identifier(`odd " name`) || identifier("") || identifier("x\x00") {
		t.Fatal("invalid identifier validation")
	}
}
