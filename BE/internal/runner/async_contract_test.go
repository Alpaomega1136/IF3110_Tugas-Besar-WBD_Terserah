package runner

import "testing"

func TestKontrakAsyncInput(t *testing.T) {
	base := AsyncOperationRequest{DatabaseID: 1, DraftID: 2, Revision: 3, ReviewID: 4, ActorID: 5, ActionType: "up"}
	cases := []struct {
		name  string
		in    AsyncOperationRequest
		valid bool
	}{
		{"up", base, true},
		{"tanpa database", func() AsyncOperationRequest { x := base; x.DatabaseID = 0; return x }(), false},
		{"tanpa review", func() AsyncOperationRequest { x := base; x.ReviewID = 0; return x }(), false},
		{"up dengan target", func() AsyncOperationRequest { x := base; x.TargetExecutionID = "E1"; return x }(), false},
		{"rollback tanpa target", func() AsyncOperationRequest { x := base; x.ActionType = "rollback"; return x }(), false},
		{"rollback", func() AsyncOperationRequest {
			x := base
			x.ActionType = "rollback"
			x.TargetExecutionID = "E1"
			return x
		}(), true},
		{"forward", func() AsyncOperationRequest {
			x := base
			x.ActionType = "forward"
			x.TargetExecutionID = "E2"
			return x
		}(), true},
		{"aksi tidak dikenal", func() AsyncOperationRequest { x := base; x.ActionType = "resume"; return x }(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.valid(); got != tc.valid {
				t.Fatalf("validasi aksi: got=%v want=%v", got, tc.valid)
			}
		})
	}
}
