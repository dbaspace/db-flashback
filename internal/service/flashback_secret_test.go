package service

import (
	"strings"
	"testing"

	secrypto "db-flashback/internal/crypto"
)

func TestFlashbackArgIsSecret(t *testing.T) {
	if !flashbackArgIsSecret(gaFlashbackTencentSecretID) || !flashbackArgIsSecret(gaFlashbackTencentSecretKey) {
		t.Fatal("tencent keys should be secret")
	}
	if flashbackArgIsSecret(gaFlashbackTencentRegion) || flashbackArgIsSecret(gaFlashbackCloudLookbackHours) {
		t.Fatal("region/lookback must stay plaintext")
	}
}

func TestMustSealArgThenOpen(t *testing.T) {
	secrypto.ResetKeyForTest()
	t.Setenv("FLASHBACK_DATA_KEY", strings.Repeat("ef", 32))
	secrypto.ResetKeyForTest()
	got, err := secrypto.MustSeal("AKIDxxx")
	if err != nil || !secrypto.IsSealed(got) {
		t.Fatalf("seal=%s err=%v", got, err)
	}
	plain, err := secrypto.Open(got)
	if err != nil || plain != "AKIDxxx" {
		t.Fatalf("open=%s err=%v", plain, err)
	}
}
