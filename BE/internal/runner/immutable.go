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

// catatExecutionAksiTx menambah event dengan SQL asli dari snapshot.
// Penentuan kelayakan rollback/forward dan approval dilakukan sebelum pemanggilan.
// Semua pemanggil wajib memegang lock database_connections dalam transaksi metadata.
func catatExecutionAksiTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision, actorID int64, executionID, actionType, sqlText string, targetExecutionID, retryOf *string) (int64, error) {
	if (actionType != "up" && actionType != "rollback" && actionType != "forward") ||
		(actionType == "up" && targetExecutionID != nil) ||
		(actionType != "up" && (targetExecutionID == nil || *targetExecutionID == "")) {
		return 0, errMigrationOrder
	}
	var version int64
	err := tx.QueryRow(ctx, `
		INSERT INTO migration_history (
			database_id,draft_id,draft_revision,version,sql,executed_by,
			status,execution_id,receipt_version,action_type,target_execution_id,retry_of_execution_id
		)
		SELECT $1,$2,$3,(SELECT COALESCE(MAX(version),0)+1 FROM migration_history WHERE database_id=$1),
		       $4,$5,'running',$6,1,$7,$8,$9
		FROM migration_revision_snapshots snap
		WHERE snap.draft_id=$2 AND snap.revision=$3
		  AND (CASE WHEN $7='rollback' THEN NOT snap.irreversible AND snap.sql_down=$4
		            ELSE snap.sql_up=$4 END)
		RETURNING version
	`, dbID, draftID, revision, sqlText, actorID, executionID, actionType, targetExecutionID, retryOf).Scan(&version)
	return version, err
}

// catatExecutionUlangTx menggunakan action Up asli dan execution parent yang gagal.
func catatExecutionUlangTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision, actorID int64, executionID, retryOf, sqlUp string) (int64, error) {
	return catatExecutionAksiTx(ctx, tx, dbID, draftID, revision, actorID, executionID, "up", sqlUp, nil, &retryOf)
}
