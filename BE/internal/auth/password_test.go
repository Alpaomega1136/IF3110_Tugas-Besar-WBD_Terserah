package auth

import "testing"

func TestPassword(t *testing.T) {
	password := "test-password-123"
	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == password {
		t.Fatal("password was not salted and hashed")
	}
	if !CheckPassword(first, password) || CheckPassword(first, "wrong password") {
		t.Fatal("password verification failed")
	}
	for _, hash := range []string{"", password, "pbkdf2-sha256$600000$invalid$invalid", "pbkdf2-sha256$999999999999$a$b"} {
		if CheckPassword(hash, password) {
			t.Fatal("malformed password hash accepted")
		}
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestLegacyPasswordCompatibility(t *testing.T) {
	// Interoperability vector generated independently with Python hashlib.
	legacy := "$pbkdf2-sha256$i=210000$AAECAwQFBgcICQoLDA0ODw$2QNDrl+R+uQV/xJCW4gZazJPobMhzqlqdv5qX/CgJFY"
	if !CheckPassword(legacy, "old-pass") || CheckPassword(legacy, "wrong") {
		t.Fatal("legacy verifier is incompatible")
	}
	if _, err := HashPassword("old-pass"); err == nil {
		t.Fatal("new passwords bypass minimum length")
	}
	upgraded, err := canonicalPasswordHash("old-pass")
	if err != nil || !CheckPassword(upgraded, "old-pass") {
		t.Fatal("legacy password cannot be upgraded")
	}
}
