package flashback

import (
	"strings"
	"testing"

	secrypto "db-flashback/internal/crypto"
)

func TestEncodeDecodeInstanceSecrets(t *testing.T) {
	secrypto.ResetKeyForTest()
	t.Setenv("FLASHBACK_DATA_KEY", strings.Repeat("ab", 32))
	secrypto.ResetKeyForTest()
	r := InstanceRow{User: "postgres", Password: "s3cret"}
	if err := encodeInstanceSecrets(&r); err != nil {
		t.Fatal(err)
	}
	if !secrypto.IsSealed(r.User) || !secrypto.IsSealed(r.Password) {
		t.Fatalf("want sealed user/pass: %s %s", r.User, r.Password)
	}
	if err := decodeInstanceSecrets(&r); err != nil {
		t.Fatal(err)
	}
	if r.User != "postgres" || r.Password != "s3cret" {
		t.Fatalf("roundtrip user=%s pass=%s", r.User, r.Password)
	}
	empty := InstanceRow{User: "u", Password: ""}
	if err := encodeInstanceSecrets(&empty); err != nil {
		t.Fatal(err)
	}
	if empty.Password != "" {
		t.Fatal("empty password must stay empty")
	}
}

func TestEncodeInstanceSecretsRequiresKey(t *testing.T) {
	secrypto.ResetKeyForTest()
	t.Setenv("FLASHBACK_DATA_KEY", "")
	secrypto.ResetKeyForTest()
	r := InstanceRow{User: "postgres", Password: "x"}
	if err := encodeInstanceSecrets(&r); err == nil {
		t.Fatal("expected error without key")
	}
}
