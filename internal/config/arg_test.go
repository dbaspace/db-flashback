package config

import "testing"

func TestFlashbackSettingsArgPrefersHubKeys(t *testing.T) {
	s := FlashbackSettings{
		TencentSecretID: "struct-id",
		Args: map[string]string{
			"flashback_tencent_secret_id":  "hub-id",
			"flashback_tencent_secret_key": "hub-key",
			"flashback_aliyun_access_key_id": "ak",
		},
	}
	if got := s.Arg("flashback_tencent_secret_id"); got != "hub-id" {
		t.Fatalf("args map should win: %s", got)
	}
	if got := s.Arg("flashback_tencent_secret_key"); got != "hub-key" {
		t.Fatalf("secret key: %s", got)
	}
	if got := s.Arg("flashback_aliyun_access_key_id"); got != "ak" {
		t.Fatalf("aliyun: %s", got)
	}
	s.Args["flashback_tencent_secret_id"] = ""
	if got := s.Arg("flashback_tencent_secret_id"); got != "struct-id" {
		t.Fatalf("empty args should fall back to field: %s", got)
	}
}
