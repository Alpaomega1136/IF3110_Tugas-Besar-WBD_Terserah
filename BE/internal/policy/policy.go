// Package policy mengelola aturan otorisasi akses database terpisah dari autentikasi.
package policy

import (
	"context"
	"errors"
)

type Access string

const (
	None Access = ""
	View Access = "view"
	Edit Access = "edit"
)

var ErrForbidden = errors.New("insufficient database permission")

func ParseAccess(raw string) (Access, bool) {
	switch Access(raw) {
	case View:
		return View, true
	case Edit:
		return Edit, true
	default:
		return None, false
	}
}

// Effective menghitung hak akses efektif dari beberapa group (edit > view > none).
func Effective(grants []Access) Access {
	access := None
	for _, grant := range grants {
		if grant == Edit {
			return Edit
		}
		if grant == View {
			access = View
		}
	}
	return access
}

func Allows(access Access, write bool) bool {
	return access == Edit || (access == View && !write)
}

// GrantFunc mengambil daftar hak akses group milik user pada suatu database.
type GrantFunc func(ctx context.Context, userID, databaseID int64) ([]Access, error)

// ResolveAccess menghitung hak akses efektif user pada satu database tanpa bypass admin.
func ResolveAccess(ctx context.Context, loadGrants GrantFunc, userID, databaseID int64) (Access, error) {
	grants, err := loadGrants(ctx, userID, databaseID)
	if err != nil {
		return None, err
	}
	return Effective(grants), nil
}

// CheckAccess memvalidasi hak baca (write=false) atau tulis (write=true) user pada database.
func CheckAccess(ctx context.Context, loadGrants GrantFunc, userID, databaseID int64, write bool) (Access, error) {
	access, err := ResolveAccess(ctx, loadGrants, userID, databaseID)
	if err != nil {
		return None, err
	}
	if !Allows(access, write) {
		return access, ErrForbidden
	}
	return access, nil
}
