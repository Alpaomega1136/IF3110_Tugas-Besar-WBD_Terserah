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
