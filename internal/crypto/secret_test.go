package crypto

import (
	"os"
	"strings"
	"testing"
)

func TestParseKey(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	k, err := ParseKey(hexKey)
	if err != nil || len(k) != 32 {
		t.Fatalf("hex: %v len=%d", err, len(k))
	}
	if _, err := ParseKey("short"); err == nil {
		t.Fatal("short key should fail")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	ResetKeyForTest()
	t.Setenv("FLASHBACK_DATA_KEY", strings.Repeat("cd", 32))
	got, err := Seal("postgres/secret")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(got) {
		t.Fatalf("not sealed: %s", got)
	}
	plain, err := Open(got)
	if err != nil || plain != "postgres/secret" {
		t.Fatalf("open=%q err=%v", plain, err)
	}
	if p, err := Open("plain-text"); err != nil || p != "plain-text" {
		t.Fatalf("plain passthrough: %q %v", p, err)
	}
}

func TestOpenWrongKey(t *testing.T) {
	ResetKeyForTest()
	t.Setenv("FLASHBACK_DATA_KEY", strings.Repeat("11", 32))
	sealed, err := Seal("x")
	if err != nil {
		t.Fatal(err)
	}
	ResetKeyForTest()
	t.Setenv("FLASHBACK_DATA_KEY", strings.Repeat("22", 32))
	if _, err := Open(sealed); err == nil {
		t.Fatal("wrong key should fail")
	}
}

func TestSealRequiresKey(t *testing.T) {
	ResetKeyForTest()
	_ = os.Unsetenv("FLASHBACK_DATA_KEY")
	if _, err := Seal("x"); err == nil {
		t.Fatal("seal without key should fail")
	}
}
