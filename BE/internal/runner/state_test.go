package runner

import (
	"errors"
	"testing"
)

func ptrInt64(x int64) *int64    { return &x }
func ptrString(x string) *string { return &x }

func eventState(draftID, version int64, executionID, status string) migrationEvent {
	return migrationEvent{migrationEntry: migrationEntry{DraftID: ptrInt64(draftID), Revision: ptrInt64(1), ExecutionID: ptrString(executionID), Version: version, CanRollback: true}, Status: status}
}

func TestPosisiMigrationUrutanLinear(t *testing.T) {
	s := migrationState{Active: []migrationEntry{}, RolledBack: []cancelledMigration{}, seen: map[int64]bool{}}
	for i, draftID := range []int64{1, 2, 3} {
		if err := s.terapkanEvent(eventState(draftID, int64(i+1), string(rune('A'+i)), "success")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.cekRollback("B"); !errors.Is(err, errMigrationOrder) {
		t.Fatalf("rollback B sebelum C ditolak: %v", err)
	}
	if err := s.cekRollback("C"); err != nil {
		t.Fatal(err)
	}
	c := eventState(3, 4, "R-C", "success")
	c.AppliedExecutionID = ptrString("C")
	if err := s.terapkanEvent(c); err != nil {
		t.Fatal(err)
	}
	if err := s.cekForward("R-C"); err != nil {
		t.Fatal(err)
	}
	if err := s.cekRollback("B"); err != nil {
		t.Fatal(err)
	}
	b := eventState(2, 5, "R-B", "success")
	b.AppliedExecutionID = ptrString("B")
	if err := s.terapkanEvent(b); err != nil {
		t.Fatal(err)
	}
	if err := s.cekForward("R-C"); !errors.Is(err, errMigrationOrder) {
		t.Fatalf("forward C harus menunggu B: %v", err)
	}
	if err := s.cekForward("R-B"); err != nil {
		t.Fatal(err)
	}
	if err := s.terapkanEvent(eventState(2, 6, "F-B", "success")); err != nil {
		t.Fatal(err)
	}
	if err := s.cekForward("R-C"); err != nil {
		t.Fatal(err)
	}
	if err := s.terapkanEvent(eventState(3, 7, "F-C", "success")); err != nil {
		t.Fatal(err)
	}
	if len(s.Active) != 3 || len(s.RolledBack) != 0 {
		t.Fatalf("posisi akhir tidak sesuai: %+v", s)
	}
	if err := s.cekRollback("F-C"); err != nil {
		t.Fatal(err)
	}
	if err := s.cekUp(3); !errors.Is(err, errMigrationMustForward) {
		t.Fatalf("SQL Up ulang tidak boleh dianggap migration baru: %v", err)
	}
}

func TestPosisiMigrationGagalPartialDanCabang(t *testing.T) {
	s := migrationState{Active: []migrationEntry{}, RolledBack: []cancelledMigration{}, seen: map[int64]bool{}}
	if err := s.terapkanEvent(eventState(1, 1, "A", "success")); err != nil {
		t.Fatal(err)
	}
	if err := s.terapkanEvent(eventState(2, 2, "G-B", "failed")); err != nil {
		t.Fatal(err)
	}
	if err := s.cekUp(2); err != nil {
		t.Fatalf("attempt gagal tidak boleh mengunci posisi aktif: %v", err)
	}
	p := eventState(2, 3, "P-B", "partial")
	if err := s.terapkanEvent(p); err != nil {
		t.Fatal(err)
	}
	if err := s.cekUp(3); !errors.Is(err, errMigrationUnresolved) {
		t.Fatalf("partial harus memblokir migration berikutnya: %v", err)
	}
	if err := s.cekRollback("A"); !errors.Is(err, errMigrationUnresolved) {
		t.Fatalf("partial harus memblokir rollback: %v", err)
	}

	c := migrationState{Active: []migrationEntry{}, RolledBack: []cancelledMigration{}, seen: map[int64]bool{}}
	if err := c.terapkanEvent(eventState(1, 1, "A", "success")); err != nil {
		t.Fatal(err)
	}
	if err := c.terapkanEvent(eventState(2, 2, "B", "success")); err != nil {
		t.Fatal(err)
	}
	r := eventState(2, 3, "R-B", "success")
	r.AppliedExecutionID = ptrString("B")
	if err := c.terapkanEvent(r); err != nil {
		t.Fatal(err)
	}
	if err := c.terapkanEvent(eventState(3, 4, "C", "success")); err != nil {
		t.Fatal(err)
	}
	if len(c.RolledBack) != 0 {
		t.Fatal("migration baru harus menutup jalur forward lama")
	}
	if err := c.cekForward("R-B"); !errors.Is(err, errMigrationOrder) {
		t.Fatalf("forward jalur lama harus ditolak: %v", err)
	}
	if err := c.terapkanEvent(eventState(2, 5, "F-B", "success")); !errors.Is(err, errMigrationStateInvalid) {
		t.Fatalf("forward lama yang dipaksakan harus terdeteksi: %v", err)
	}
}

func TestPosisiMigrationValidasiRiwayat(t *testing.T) {
	s := migrationState{Active: []migrationEntry{}, RolledBack: []cancelledMigration{}, seen: map[int64]bool{}}
	if err := s.terapkanEvent(eventState(1, 1, "A", "success")); err != nil {
		t.Fatal(err)
	}
	if err := s.terapkanEvent(eventState(2, 2, "B", "success")); err != nil {
		t.Fatal(err)
	}
	r := eventState(1, 3, "R-A", "success")
	r.AppliedExecutionID = ptrString("A")
	if err := s.terapkanEvent(r); !errors.Is(err, errMigrationStateInvalid) {
		t.Fatalf("rollback bukan terakhir harus ditolak: %v", err)
	}

	i := migrationState{Active: []migrationEntry{}, RolledBack: []cancelledMigration{}, seen: map[int64]bool{}}
	x := eventState(1, 1, "I", "success")
	x.CanRollback = false
	if err := i.terapkanEvent(x); err != nil {
		t.Fatal(err)
	}
	if err := i.cekRollback("I"); !errors.Is(err, errMigrationIrreversible) {
		t.Fatalf("irreversible harus ditolak: %v", err)
	}
	i.siapkanAksi()
	if i.NextRollback != nil || i.NextForward != nil {
		t.Fatal("tidak boleh menawarkan rollback irreversible")
	}
}
