package crypto

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("flashback")
	if err != nil || hash == "" || hash == "flashback" {
		t.Fatalf("hash=%s err=%v", hash, err)
	}
	if !CheckPassword(hash, "flashback") {
		t.Fatal("same password should match")
	}
	if CheckPassword(hash, "wrong") || CheckPassword(hash, "") {
		t.Fatal("wrong password must not match")
	}
	if _, err := HashPassword("  "); err == nil {
		t.Fatal("empty password should fail")
	}
}
