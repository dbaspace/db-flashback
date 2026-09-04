package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewDataKeyHex(t *testing.T) {
	a, err := NewDataKeyHex()
	if err != nil || len(a) != 64 {
		t.Fatalf("hex=%s err=%v", a, err)
	}
	b, err := NewDataKeyHex()
	if err != nil || a == b {
		t.Fatal("keys should be random")
	}
}

func TestEnsureDataKeyWritesOnce(t *testing.T) {
	t.Setenv("FLASHBACK_DATA_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("flashback:\n  data_key: \"\"\n  workdir: /tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &SvrConfig{}
	ok, err := EnsureDataKey(cfg, path)
	if err != nil || !ok || len(cfg.Flashback.DataKey) != 64 {
		t.Fatalf("first gen ok=%v key=%s err=%v", ok, cfg.Flashback.DataKey, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), cfg.Flashback.DataKey) {
		t.Fatalf("key not persisted: %s", raw)
	}
	first := cfg.Flashback.DataKey
	cfg2 := &SvrConfig{Flashback: FlashbackSettings{DataKey: first}}
	ok, err = EnsureDataKey(cfg2, path)
	if err != nil || ok || cfg2.Flashback.DataKey != first {
		t.Fatalf("second should keep key ok=%v err=%v", ok, err)
	}
}

func TestEnsureDataKeyEnvWins(t *testing.T) {
	t.Setenv("FLASHBACK_DATA_KEY", strings.Repeat("aa", 32))
	cfg := &SvrConfig{}
	ok, err := EnsureDataKey(cfg, "/tmp/not-used.yaml")
	if err != nil || ok || cfg.Flashback.DataKey != strings.Repeat("aa", 32) {
		t.Fatalf("env should win: ok=%v key=%s err=%v", ok, cfg.Flashback.DataKey, err)
	}
}
