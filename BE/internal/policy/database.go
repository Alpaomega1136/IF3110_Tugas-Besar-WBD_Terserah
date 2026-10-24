package policy

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseAccess reloads group grants on each call, scoped to one target.
func DatabaseAccess(ctx context.Context, db *pgxpool.Pool, userID, databaseID int64) (Access, error) {
	rows, err := db.Query(ctx, `SELECT p.access_level FROM group_database_permissions p
		JOIN user_groups ug ON ug.group_id=p.group_id JOIN users u ON u.id=ug.user_id
		WHERE ug.user_id=$1 AND p.database_id=$2 AND u.is_active=true`, userID, databaseID)
	if err != nil {
		return None, err
	}
	defer rows.Close()
	var grants []Access
	for rows.Next() {
		var a Access
		if err = rows.Scan(&a); err != nil {
			return None, err
		}
		grants = append(grants, a)
	}
	return Effective(grants), rows.Err()
}
