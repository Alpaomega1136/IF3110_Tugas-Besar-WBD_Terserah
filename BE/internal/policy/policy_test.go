package policy

import (
	"context"
	"errors"
	"testing"
)

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

func TestCheckAccessAndCrossDatabaseIsolation(t *testing.T) {
	if _, ok := ParseAccess("owner"); ok {
		t.Fatal("invalid access level accepted")
	}
	grantsMap := map[[2]int64][]Access{
		{10, 1}: {View, Edit},
		{10, 2}: {View},
	}
	loader := func(_ context.Context, userID, databaseID int64) ([]Access, error) {
		return grantsMap[[2]int64{userID, databaseID}], nil
	}
	if acc, err := CheckAccess(context.Background(), loader, 10, 1, true); err != nil || acc != Edit {
		t.Fatalf("expected edit on db 1, got %q err=%v", acc, err)
	}
	if _, err := CheckAccess(context.Background(), loader, 10, 2, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected write forbidden on db 2 (view only), got %v", err)
	}
	if acc, err := CheckAccess(context.Background(), loader, 10, 2, false); err != nil || acc != View {
		t.Fatalf("expected view allowed on db 2, got %q err=%v", acc, err)
	}
	if _, err := CheckAccess(context.Background(), loader, 10, 3, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected forbidden on db 3, got %v", err)
	}
}
