// Package policy owns authorization independently of authentication.
package policy

type Access string

const (
	None Access = ""
	View Access = "view"
	Edit Access = "edit"
)

// Effective combines group grants for ONE database. No admin bypass is implied.
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
