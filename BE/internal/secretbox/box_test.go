package secretbox

import (
	"bytes"
	"testing"
)

func TestBox(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	box, _ := New(bytes.Repeat([]byte{1}, 32))
	value := []byte("target password")
	first := box.Seal(value, "target:1")
	second := box.Seal(value, "target:1")
	if bytes.Equal(first, second) || bytes.Contains(first, value) {
		t.Fatal("encryption is deterministic or exposes plaintext")
	}
	got, err := box.Open(first, "target:1")
	if err != nil || !bytes.Equal(got, value) {
		t.Fatal("round trip failed")
	}
	if _, err = box.Open(first, "target:2"); err == nil {
		t.Fatal("credential can be moved between targets")
	}
	first[len(first)-1] ^= 1
	if _, err = box.Open(first, "target:1"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err = box.Open([]byte{1}, "target:1"); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
}
