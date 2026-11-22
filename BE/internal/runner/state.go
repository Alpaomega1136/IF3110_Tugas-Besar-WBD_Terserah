package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var errMigrationOrder = errors.New("urutan migration tidak valid")
var errMigrationIrreversible = errors.New("migration tidak dapat di-rollback")
var errMigrationUnresolved = errors.New("masih ada eksekusi migration yang belum terselesaikan")
var errMigrationStateInvalid = errors.New("riwayat migration tidak konsisten")
var errMigrationMustForward = errors.New("migration yang pernah berhasil harus diterapkan kembali melalui forward")

type migrationEntry struct {
	DraftID     *int64  `json:"draft_id,string"`
	Revision    *int64  `json:"draft_revision,string"`
	ExecutionID *string `json:"execution_id"`
	Version     int64   `json:"version,string"`
	CanRollback bool    `json:"can_rollback"`
}

type cancelledMigration struct {
	Applied             migrationEntry `json:"applied"`
	RollbackExecutionID string         `json:"rollback_execution_id"`
}

type migrationState struct {
	DatabaseID    int64                `json:"database_id,string"`
	Active        []migrationEntry     `json:"active"`
	RolledBack    []cancelledMigration `json:"rolled_back"`
	HasUnresolved bool                 `json:"has_unresolved"`
	NextRollback  *migrationEntry      `json:"next_rollback,omitempty"`
	NextForward   *cancelledMigration  `json:"next_forward,omitempty"`
	seen          map[int64]bool
}

type migrationEvent struct {
	migrationEntry
	Status             string
	AppliedExecutionID *string
}

type migrationQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// bacaPosisiMigration menyusun posisi berdasarkan event committed dan pasangan rollback.
// Saat dipakai untuk memutuskan eksekusi, pemanggil wajib mengunci database_connections
// dalam transaksi metadata agar posisi tidak berubah di antara pemeriksaan dan pencatatan.
func bacaPosisiMigration(ctx context.Context, db migrationQueryer, dbID int64) (migrationState, error) {
	rows, err := db.Query(ctx, `
		SELECT h.draft_id, h.draft_revision, h.execution_id, h.version, h.status,
		       pair.applied_execution_id,
		       COALESCE(NOT snap.irreversible AND snap.sql_down IS NOT NULL, false)
		FROM migration_history h
		LEFT JOIN migration_rollback_pairs pair
		  ON pair.database_id = h.database_id AND pair.rollback_execution_id = h.execution_id
		LEFT JOIN migration_revision_snapshots snap
		  ON snap.draft_id = h.draft_id AND snap.revision = h.draft_revision
		WHERE h.database_id = $1
		ORDER BY h.version
	`, dbID)
	if err != nil {
		return migrationState{}, err
	}
	defer rows.Close()

	state := migrationState{DatabaseID: dbID, Active: []migrationEntry{}, RolledBack: []cancelledMigration{}, seen: make(map[int64]bool)}
	for rows.Next() {
		var event migrationEvent
		if err := rows.Scan(&event.DraftID, &event.Revision, &event.ExecutionID, &event.Version,
			&event.Status, &event.AppliedExecutionID, &event.CanRollback); err != nil {
			return migrationState{}, err
		}
		if err := state.terapkanEvent(event); err != nil {
			return migrationState{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return migrationState{}, err
	}
	state.siapkanAksi()
	return state, nil
}

// terapkanEvent hanya mengubah posisi untuk eksekusi sukses. Attempt gagal tetap
// berada di history, sedangkan running/unknown/partial menghalangi aksi baru.
func (s *migrationState) terapkanEvent(event migrationEvent) error {
	if event.Status != "success" {
		if event.Status != "failed" {
			s.HasUnresolved = true
		}
		return nil
	}
	if event.AppliedExecutionID != nil {
		if len(s.Active) == 0 || event.ExecutionID == nil || *event.ExecutionID == "" {
			return errMigrationStateInvalid
		}
		last := s.Active[len(s.Active)-1]
		if last.ExecutionID == nil || *last.ExecutionID != *event.AppliedExecutionID ||
			!identikMigration(last, event.migrationEntry) {
			return fmt.Errorf("%w: rollback tidak membatalkan execution aktif terakhir", errMigrationStateInvalid)
		}
		s.Active = s.Active[:len(s.Active)-1]
		s.RolledBack = append(s.RolledBack, cancelledMigration{Applied: last, RollbackExecutionID: *event.ExecutionID})
		return nil
	}
	if event.DraftID != nil {
		for _, active := range s.Active {
			if active.DraftID != nil && *active.DraftID == *event.DraftID {
				return fmt.Errorf("%w: migration aktif diterapkan dua kali", errMigrationStateInvalid)
			}
		}
	}
	forward := false
	if len(s.RolledBack) > 0 {
		last := s.RolledBack[len(s.RolledBack)-1]
		if identikMigration(last.Applied, event.migrationEntry) {
			// Forward harus dimulai dari migration yang paling akhir di-rollback.
			forward = true
			s.RolledBack = s.RolledBack[:len(s.RolledBack)-1]
		} else {
			for _, item := range s.RolledBack {
				if identikMigration(item.Applied, event.migrationEntry) {
					return fmt.Errorf("%w: forward melompati migration yang di-rollback", errMigrationStateInvalid)
				}
			}
			// Migration baru setelah rollback memulai cabang linear baru;
			// execution yang dibatalkan tetap ada dalam migration history.
			s.RolledBack = s.RolledBack[:0]
		}
	}
	if event.DraftID != nil {
		if s.seen[*event.DraftID] && !forward {
			return fmt.Errorf("%w: migration lama diterapkan tanpa forward yang sah", errMigrationStateInvalid)
		}
		s.seen[*event.DraftID] = true
	}
	s.Active = append(s.Active, event.migrationEntry)
	return nil
}

func identikMigration(a, b migrationEntry) bool {
	return a.DraftID != nil && b.DraftID != nil && a.Revision != nil && b.Revision != nil &&
		*a.DraftID == *b.DraftID && *a.Revision == *b.Revision
}

func (s *migrationState) siapkanAksi() {
	if s.HasUnresolved {
		return
	}
	if len(s.Active) > 0 {
		last := s.Active[len(s.Active)-1]
		if last.ExecutionID != nil && *last.ExecutionID != "" && last.CanRollback {
			s.NextRollback = &last
		}
	}
	if len(s.RolledBack) > 0 {
		last := s.RolledBack[len(s.RolledBack)-1]
		s.NextForward = &last
	}
}

// cekUp membedakan migration baru dari forward dan mencegah duplikasi versi aktif.
func (s migrationState) cekUp(draftID int64) error {
	if s.HasUnresolved {
		return errMigrationUnresolved
	}
	if s.seen[draftID] {
		return errMigrationMustForward
	}
	return nil
}

// cekRollback memastikan hanya execution aktif terakhir yang reversible yang boleh dibatalkan.
func (s migrationState) cekRollback(executionID string) error {
	if s.HasUnresolved {
		return errMigrationUnresolved
	}
	if len(s.Active) == 0 {
		return errMigrationOrder
	}
	last := s.Active[len(s.Active)-1]
	if last.ExecutionID == nil || *last.ExecutionID != executionID || executionID == "" {
		return errMigrationOrder
	}
	if !last.CanRollback {
		return errMigrationIrreversible
	}
	return nil
}

// cekForward mengacu pada execution rollback terakhir, bukan nama migration.
func (s migrationState) cekForward(rollbackExecutionID string) error {
	if s.HasUnresolved {
		return errMigrationUnresolved
	}
	if len(s.RolledBack) == 0 || rollbackExecutionID == "" ||
		s.RolledBack[len(s.RolledBack)-1].RollbackExecutionID != rollbackExecutionID {
		return errMigrationOrder
	}
	return nil
}
