package policy

import "testing"

func TestEffectiveAccess(t *testing.T) {
	for _, tc := range []struct {
		name        string
		grants      []Access
		want        Access
		read, write bool
	}{
		{"no grant", nil, None, false, false},
		{"view", []Access{View}, View, true, false},
		{"highest grant wins", []Access{View, Edit, View}, Edit, true, true},
		{"unknown denied", []Access{"admin"}, None, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Effective(tc.grants)
			if got != tc.want || Allows(got, false) != tc.read || Allows(got, true) != tc.write {
				t.Fatalf("incorrect permission for %v: %q", tc.grants, got)
			}
		})
	}
}
