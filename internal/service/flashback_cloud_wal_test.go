package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mdmmodel "db-flashback/internal/mdmmodel"
)

func TestFlashbackResolveWALSource(t *testing.T) {
	company := &mdmmodel.ResourceDbsInfo{
		Address: "pg.shop.example.com:5432",
		Name:    "beijing-shop",
		Tags: map[string]interface{}{
			"domain": "pg.shop.example.com",
			"pg":     "postgres-ornl8fgs",
		},
	}
	src := flashbackResolveWALSource(company, "", "pg.shop.example.com")
	if src.Kind != flashbackWALSourceCloudTencent || src.InstanceID != "postgres-ornl8fgs" {
		t.Fatalf("pg tag: %+v", src)
	}
	if !strings.Contains(src.Reason, "pg=") {
		t.Fatalf("reason=%s", src.Reason)
	}

	noTag := &mdmmodel.ResourceDbsInfo{Address: "pg.shop.example.com:5432", Name: "beijing-shop"}
	src = flashbackResolveWALSource(noTag, "", "pg.shop.example.com")
	if src.Kind != flashbackWALSourceSelf {
		t.Fatalf("company domain without pg tag should be self: %+v", src)
	}

	src = flashbackResolveWALSource(noTag, "postgres-abc123xy", "")
	if src.Kind != flashbackWALSourceCloudTencent || src.InstanceID != "postgres-abc123xy" {
		t.Fatalf("override: %+v", src)
	}

	tencentHost := &mdmmodel.ResourceDbsInfo{Address: "cdb-xxxx.postgres.tencentcdb.com:5432"}
	src = flashbackResolveWALSource(tencentHost, "")
	if src.Kind != flashbackWALSourceTencentNeedID {
		t.Fatalf("tencent host without id: %+v", src)
	}

	ali := &mdmmodel.ResourceDbsInfo{
		Address: "10.1.1.1:5432",
		Tags:    map[string]interface{}{"vendor": "aliyun"},
	}
	src = flashbackResolveWALSource(ali, "")
	if src.Kind != flashbackWALSourceCloudOther || src.Vendor != flashbackVendorAliyun {
		t.Fatalf("aliyun: %+v", src)
	}

	self := &mdmmodel.ResourceDbsInfo{Address: "10.0.0.8:5432"}
	src = flashbackResolveWALSource(self, "", "pg.internal.example.com")
	if src.Kind != flashbackWALSourceSelf {
		t.Fatalf("self: %+v", src)
	}

	fvTencent := &mdmmodel.ResourceDbsInfo{
		Address: "pg.shop.example.com:5432",
		Tags: map[string]interface{}{
			"flash_vendor": "tencent",
			"pg":           "postgres-ornl8fgs",
		},
	}
	src = flashbackResolveWALSource(fvTencent, "")
	if src.Kind != flashbackWALSourceCloudTencent || src.Vendor != flashbackVendorTencent || src.InstanceID != "postgres-ornl8fgs" {
		t.Fatalf("flash_vendor tencent: %+v", src)
	}
	if !strings.Contains(src.Reason, "flash_vendor") {
		t.Fatalf("reason=%s", src.Reason)
	}

	fvNeedID := &mdmmodel.ResourceDbsInfo{
		Address: "pg.shop.example.com:5432",
		Tags:    map[string]interface{}{"flash_vendor": "tencent"},
	}
	src = flashbackResolveWALSource(fvNeedID, "")
	if src.Kind != flashbackWALSourceTencentNeedID {
		t.Fatalf("flash_vendor tencent without pg: %+v", src)
	}

	fvAli := &mdmmodel.ResourceDbsInfo{
		Address: "pg.shop.example.com:5432",
		Tags: map[string]interface{}{
			"flash_vendor": "aliyun",
			"pg":           "postgres-ornl8fgs",
		},
	}
	src = flashbackResolveWALSource(fvAli, "")
	if src.Kind != flashbackWALSourceCloudOther || src.Vendor != flashbackVendorAliyun {
		t.Fatalf("flash_vendor aliyun wins over pg=: %+v", src)
	}

	fvHW := &mdmmodel.ResourceDbsInfo{Tags: map[string]interface{}{"flash_vendor": "huawei"}}
	src = flashbackResolveWALSource(fvHW, "")
	if src.Kind != flashbackWALSourceCloudOther || src.Vendor != flashbackVendorHuawei {
		t.Fatalf("flash_vendor huawei: %+v", src)
	}

	fvAWS := &mdmmodel.ResourceDbsInfo{Tags: map[string]interface{}{"flash_vendor": "aws"}}
	src = flashbackResolveWALSource(fvAWS, "")
	if src.Kind != flashbackWALSourceCloudOther || src.Vendor != flashbackVendorAWS {
		t.Fatalf("flash_vendor aws: %+v", src)
	}

	fvBad := &mdmmodel.ResourceDbsInfo{Tags: map[string]interface{}{"flash_vendor": "foo"}}
	src = flashbackResolveWALSource(fvBad, "")
	if src.Kind != flashbackWALSourceCloudOther || !strings.Contains(src.Reason, "不是规范值") {
		t.Fatalf("invalid flash_vendor: %+v", src)
	}
}

func TestFlashbackAllowedArgKey(t *testing.T) {
	if !flashbackAllowedArgKey(gaFlashbackTencentSecretID) || !flashbackAllowedArgKey(gaFlashbackAliyunAccessKeyID) {
		t.Fatal("hub keys should be allowed")
	}
	if flashbackAllowedArgKey("unknown_key") || flashbackAllowedArgKey("") {
		t.Fatal("unknown key must be rejected")
	}
}

func TestFlashbackCloudVendorKeyPair(t *testing.T) {
	idKey, keyKey, _, _, err := flashbackCloudVendorKeyPair(flashbackVendorTencent)
	if err != nil || idKey != gaFlashbackTencentSecretID || keyKey != gaFlashbackTencentSecretKey {
		t.Fatalf("tencent: %s %s err=%v", idKey, keyKey, err)
	}
	idKey, keyKey, _, _, err = flashbackCloudVendorKeyPair(flashbackVendorAliyun)
	if err != nil || idKey != gaFlashbackAliyunAccessKeyID || keyKey != gaFlashbackAliyunAccessKeySecret {
		t.Fatalf("aliyun: %s %s err=%v", idKey, keyKey, err)
	}
	idKey, keyKey, _, _, err = flashbackCloudVendorKeyPair(flashbackVendorHuawei)
	if err != nil || idKey != gaFlashbackHuaweiAccessKeyID || keyKey != gaFlashbackHuaweiSecretAccessKey {
		t.Fatalf("huawei: %s %s err=%v", idKey, keyKey, err)
	}
	idKey, keyKey, _, _, err = flashbackCloudVendorKeyPair(flashbackVendorAWS)
	if err != nil || idKey != gaFlashbackAWSAccessKeyID || keyKey != gaFlashbackAWSSecretAccessKey {
		t.Fatalf("aws: %s %s err=%v", idKey, keyKey, err)
	}
	if _, _, _, _, err = flashbackCloudVendorKeyPair("unknown"); err == nil {
		t.Fatal("expected unknown vendor error")
	}
}

func TestFlashbackCloudVendorCreds(t *testing.T) {
	t.Setenv("FLASHBACK_TENCENT_SECRET_ID", "tid")
	t.Setenv("FLASHBACK_TENCENT_SECRET_KEY", "tkey")
	id, key, err := flashbackCloudVendorCreds(context.Background(), flashbackVendorTencent)
	if err != nil || id != "tid" || key != "tkey" {
		t.Fatalf("tencent env: id=%s key=%s err=%v", id, key, err)
	}

	t.Setenv("FLASHBACK_ALIYUN_ACCESS_KEY_ID", "")
	t.Setenv("FLASHBACK_ALIYUN_ACCESS_KEY_SECRET", "")
	_, _, err = flashbackCloudVendorCreds(context.Background(), flashbackVendorAliyun)
	if err == nil || !strings.Contains(err.Error(), gaFlashbackAliyunAccessKeyID) || !strings.Contains(err.Error(), gaFlashbackAliyunAccessKeySecret) {
		t.Fatalf("aliyun missing keys should name global_args: %v", err)
	}
	if strings.Contains(err.Error(), gaFlashbackTencentSecretID) {
		t.Fatal("aliyun must not mention tencent global_args")
	}
}

func TestFlashbackNormalizeTencentPGID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres-ornl8fgs", "postgres-ornl8fgs"},
		{"pg:postgres-ORNL8FGS", "postgres-ornl8fgs"},
		{"beijing-shop", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := flashbackNormalizeTencentPGID(tc.in); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFlashbackCloudPkgOverlaps(t *testing.T) {
	from := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ok := flashbackCloudWALObject{
		State:      "finished",
		StartTime:  time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC),
		FinishTime: time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC),
	}
	if !flashbackCloudPkgOverlaps(ok, from, to) {
		t.Fatal("should overlap")
	}
	late := ok
	late.StartTime = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	late.FinishTime = time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)
	if flashbackCloudPkgOverlaps(late, from, to) {
		t.Fatal("late pkg should not overlap")
	}
	filtered := flashbackFilterCloudWALPkgs([]flashbackCloudWALObject{
		ok,
		{State: "running", StartTime: from, FinishTime: to},
		late,
	}, from, to)
	if len(filtered) != 1 {
		t.Fatalf("filtered=%d", len(filtered))
	}
}

func TestFlashbackCloudDownloadWindow(t *testing.T) {
	target := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	cp := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	from, to := flashbackCloudDownloadWindow(target, end, cp, 2*time.Hour)
	if !from.Equal(cp.Add(-2 * time.Hour)) {
		t.Fatalf("from=%s", from)
	}
	if !to.Equal(end) {
		t.Fatalf("to=%s", to)
	}
}

func TestFlashbackCloudApplyDownloadLag(t *testing.T) {
	from := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	gotFrom, gotTo := flashbackCloudApplyDownloadLag(from, to, flashbackCloudOSSLag)
	if !gotFrom.Equal(from.Add(-3 * time.Minute)) {
		t.Fatalf("from=%s", gotFrom)
	}
	if !gotTo.Equal(to.Add(3 * time.Minute)) {
		t.Fatalf("to=%s", gotTo)
	}
	sameFrom, sameTo := flashbackCloudApplyDownloadLag(from, to, 0)
	if !sameFrom.Equal(from) || !sameTo.Equal(to) {
		t.Fatalf("lag<=0 should keep window from=%s to=%s", sameFrom, sameTo)
	}
}

func TestFlashbackCloudWaitDownloadLag(t *testing.T) {
	ctx := context.Background()
	if err := flashbackCloudWaitDownloadLag(ctx, time.Now().Add(-time.Hour), flashbackCloudOSSLag); err != nil {
		t.Fatalf("past end should not wait: %v", err)
	}
	if err := flashbackCloudWaitDownloadLag(ctx, time.Now(), 0); err != nil {
		t.Fatalf("lag<=0: %v", err)
	}
	cancel, stop := context.WithCancel(ctx)
	stop()
	if err := flashbackCloudWaitDownloadLag(cancel, time.Now().Add(time.Hour), flashbackCloudOSSLag); err == nil {
		t.Fatal("canceled ctx should fail")
	}
}

func TestFlashbackUnpackCloudWALTarGz(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "inc.tar.gz")
	name := "000000010000000000000001"
	if err := writeTestWALTarGz(src, name, []byte("WALFAKE")); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "wal")
	files, err := flashbackUnpackCloudWAL(src, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != name {
		t.Fatalf("files=%+v", files)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, name))
	if err != nil || string(raw) != "WALFAKE" {
		t.Fatalf("content=%q err=%v", raw, err)
	}
}

func TestFlashbackTC3Headers(t *testing.T) {
	h := flashbackTC3Headers("AKIDtest", "secret", "postgres", "postgres.tencentcloudapi.com",
		"DescribeLogBackups", "2017-03-12", "ap-beijing", []byte(`{"Limit":1}`), time.Unix(1551113065, 0).UTC())
	if !strings.HasPrefix(h["Authorization"], "TC3-HMAC-SHA256 Credential=AKIDtest/") {
		t.Fatalf("auth=%s", h["Authorization"])
	}
	if h["X-TC-Action"] != "DescribeLogBackups" || h["X-TC-Region"] != "ap-beijing" {
		t.Fatalf("headers=%v", h)
	}
}

func TestFlashbackTencentListByTimeMock(t *testing.T) {
	p := &flashbackTencentWALProvider{
		secretID: "id", secretKey: "key", region: "ap-beijing",
		do: func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("X-TC-Action") != "DescribeLogBackups" {
				t.Fatalf("action=%s", req.Header.Get("X-TC-Action"))
			}
			body := `{
			  "Response": {
			    "TotalCount": 1,
			    "LogBackupSet": [{
			      "Id": "2628bcde-ce13-554a-b47d-2b15187a02ec",
			      "DBInstanceId": "postgres-ornl8fgs",
			      "StartTime": "2026-09-01 10:00:00",
			      "FinishTime": "2026-09-01 11:00:00",
			      "Size": 16783360,
			      "Name": "x.tar.gz",
			      "State": "finished"
			    }]
			  }
			}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		},
	}
	from := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	pkgs, err := p.ListByTime(context.Background(), flashbackCloudWALSpec{
		InstanceID: "postgres-ornl8fgs", From: from, To: to,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].ID != "2628bcde-ce13-554a-b47d-2b15187a02ec" {
		t.Fatalf("pkgs=%+v", pkgs)
	}
}

func TestFlashbackLooksLikeTencentHost(t *testing.T) {
	if !flashbackLooksLikeTencentHost("cdb-xxxx.postgres.tencentcdb.com") {
		t.Fatal("expected tencent")
	}
	if flashbackLooksLikeTencentHost("pg.shop.example.com") {
		t.Fatal("company domain is not tencent host")
	}
}

func writeTestWALTarGz(path, name string, payload []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0600, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(payload); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func TestFlashbackLimitReaderUnlimited(t *testing.T) {
	r := flashbackLimitReader(bytes.NewReader([]byte("abc")), 0)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "abc" {
		t.Fatalf("got %q err=%v", b, err)
	}
}

func TestFlashbackResolveTencentRegion(t *testing.T) {
	t.Setenv("FLASHBACK_TENCENT_REGION", "")
	ctx := context.Background()

	shop := &mdmmodel.ResourceDbsInfo{
		Address: "pg.shop.example.com:5432",
		Tags: map[string]interface{}{
			"idc":          "beijing",
			"pg":           "postgres-ornl8fgs",
			"flash_region": "ap-beijing",
		},
	}
	got, err := flashbackResolveTencentRegion(ctx, shop, "")
	if err != nil || got.Region != "ap-beijing" || !strings.Contains(got.Reason, "flash_region") {
		t.Fatalf("flash_region: %+v err=%v", got, err)
	}

	short := &mdmmodel.ResourceDbsInfo{Tags: map[string]interface{}{"flash_region": "beijing"}}
	got, err = flashbackResolveTencentRegion(ctx, short, "")
	if err != nil || got.Region != "ap-beijing" {
		t.Fatalf("short flash_region: %+v err=%v", got, err)
	}

	idcOnly := &mdmmodel.ResourceDbsInfo{Tags: map[string]interface{}{"idc": "beijing"}}
	got, err = flashbackResolveTencentRegion(ctx, idcOnly, "")
	if err != nil || got.Region != "ap-beijing" || !strings.Contains(got.Reason, "idc") {
		t.Fatalf("idc fallback: %+v err=%v", got, err)
	}

	bj := &mdmmodel.ResourceDbsInfo{
		RegionID: "bj",
		Region:   &mdmmodel.Region{Name: "bj"},
	}
	got, err = flashbackResolveTencentRegion(ctx, bj, "")
	if err != nil || got.Region != "ap-beijing" {
		t.Fatalf("Region=bj: %+v err=%v", got, err)
	}

	skipIDC := &mdmmodel.ResourceDbsInfo{
		Tags:     map[string]interface{}{"idc": "dev"},
		RegionID: "sh",
	}
	got, err = flashbackResolveTencentRegion(ctx, skipIDC, "")
	if err != nil || got.Region != "ap-shanghai" {
		t.Fatalf("skip idc=dev: %+v err=%v", got, err)
	}

	_, err = flashbackResolveTencentRegion(ctx, &mdmmodel.ResourceDbsInfo{Name: "x"}, "")
	if err == nil {
		t.Fatal("expected fail without region")
	}

	got, err = flashbackResolveTencentRegion(ctx, &mdmmodel.ResourceDbsInfo{Name: "x"}, "ap-guangzhou")
	if err != nil || got.Region != "ap-guangzhou" {
		t.Fatalf("override: %+v err=%v", got, err)
	}
}

func TestFlashbackNormalizeAPIRegion(t *testing.T) {
	if flashbackNormalizeAPIRegion("ap-beijing", nil) != "ap-beijing" {
		t.Fatal("ap-beijing")
	}
	if flashbackNormalizeAPIRegion("BJ", nil) != "ap-beijing" {
		t.Fatal("BJ")
	}
	if flashbackNormalizeAPIRegion("dev", nil) != "" {
		t.Fatal("dev should skip")
	}
}
