package runner

import (
	"context"
	"errors"
	"testing"
)

func TestCatatExecutionAksiKoordinatWajib(t *testing.T) {
	target := "rollback-1"
	kasus := []struct {
		nama, aksi string
		target     *string
	}{
		{"aksi tidak dikenal", "delete", nil},
		{"Up tidak boleh mempunyai target", "up", &target},
		{"rollback wajib mempunyai target", "rollback", nil},
		{"forward wajib mempunyai target", "forward", nil},
	}
	for _, k := range kasus {
		t.Run(k.nama, func(t *testing.T) {
			_, err := catatExecutionAksiTx(context.Background(), nil, 1, 1, 1, 1, "new", k.aksi, "SELECT 1", k.target, nil)
			if !errors.Is(err, errMigrationOrder) {
				t.Fatalf("koordinat tidak ditolak: %v", err)
			}
		})
	}
}
