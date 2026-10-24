package runner

import "testing"

func TestReceiptSignatureBindsAttempt(t *testing.T) {
	s := Service{key: []byte("01234567890123456789012345678901")}
	want := s.signature(1, "attempt", 1, "SELECT 1", 2)
	for _, got := range []string{s.signature(2, "attempt", 1, "SELECT 1", 2), s.signature(1, "other", 1, "SELECT 1", 2), s.signature(1, "attempt", 2, "SELECT 1", 2), s.signature(1, "attempt", 1, "SELECT 2", 2), s.signature(1, "attempt", 1, "SELECT 1", 3), (&Service{key: []byte("other key")}).signature(1, "attempt", 1, "SELECT 1", 2)} {
		if got == want {
			t.Fatal("signature did not bind every field and key")
		}
	}
	if len(want) != 64 || want != s.signature(1, "attempt", 1, "SELECT 1", 2) {
		t.Fatal("unstable receipt signature")
	}
}
