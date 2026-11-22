package runner

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// catatExecutionPertamaTx menyimpan attempt pertama dan snapshot revisinya
// dalam satu transaksi metadata agar tidak ada history tanpa snapshot.
// Pemanggil wajib mengunci baris database dan draft sebelum memanggil fungsi ini.
func catatExecutionPertamaTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision, actorID int64, executionID, sqlUp string, sqlDown *string, irreversible bool, reason *string) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `
        INSERT INTO migration_history (
            database_id, draft_id, draft_revision, version, sql,
            executed_by, status, execution_id, receipt_version
        )
        SELECT $1, $2, $3, COALESCE(MAX(version), 0) + 1, $4, $5, 'running', $6, 1
        FROM migration_history
        WHERE database_id = $1
        RETURNING version
    `, dbID, draftID, revision, sqlUp, actorID, executionID).Scan(&version)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `
        INSERT INTO migration_revision_snapshots (
            draft_id, revision, sql_up, sql_down, irreversible,
            irreversible_reason, first_execution_id
        ) VALUES ($1, $2, $3, $4, $5, $6, $7)
    `, draftID, revision, sqlUp, sqlDown, irreversible, reason, executionID)
	if err != nil {
		return 0, err
	}
	return version, nil
}
