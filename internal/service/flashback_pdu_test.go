package service

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"db-flashback/internal/service/dto"
)

func TestFlashbackValidatePDUReq(t *testing.T) {
	req := &dto.FlashbackTaskReq{
		Engine: "pdu", Database: "app", TargetTime: time.Now().Add(-time.Hour).Format(time.RFC3339),
		PGDataPath: "/tmp/pgdata", PDUScene: "wal_delete",
	}
	if err := flashbackValidateReq(req); err == nil {
		t.Fatal("expected archive_dest required")
	}
	req.ArchiveDest = "/tmp/wal"
	if err := flashbackValidateReq(req); err != nil {
		t.Fatal(err)
	}
	if req.PDUScene != flashbackPDUSceneWALDelete {
		t.Fatalf("scene=%s", req.PDUScene)
	}
}

func TestFlashbackValidateReqStillNeedsInstance(t *testing.T) {
	req := &dto.FlashbackTaskReq{Database: "app", TargetTime: time.Now().Format(time.RFC3339)}
	if err := flashbackValidateReq(req); err == nil {
		t.Fatal("online still needs instance_id")
	}
}

func TestFlashbackPDUPathAllowed(t *testing.T) {
	ctx := context.Background()
	if err := flashbackPDUPathAllowed(ctx, "/tmp/pgdata-copy"); err != nil {
		t.Fatal(err)
	}
	if err := flashbackPDUPathAllowed(ctx, "relative/path"); err == nil {
		t.Fatal("relative should fail")
	}
}

func TestFlashbackPDUFilterWALNames(t *testing.T) {
	files := []flashbackWALFile{
		{Name: "000000010000000000000001"},
		{Name: "000000010000000000000002"},
		{Name: "000000010000000000000003"},
	}
	got := flashbackPDUFilterWALNames(files, "000000010000000000000002", "000000010000000000000002")
	if len(got) != 1 || got[0].Name != "000000010000000000000002" {
		t.Fatalf("%v", got)
	}
}

func TestFlashbackDedupeSortColumns(t *testing.T) {
	got := flashbackDedupeSortColumns([]flashbackColumn{
		{Name: "cname", AttNum: 2, TypeOID: 25},
		{Name: "id", AttNum: 1, TypeOID: 23},
		{Name: "id", AttNum: 1, TypeOID: 0, Dropped: true},
	})
	if len(got) != 2 || got[0].Name != "id" || got[1].Name != "cname" {
		t.Fatalf("%+v", got)
	}
	if got[0].TypeName != "int4" || got[1].TypeName != "text" {
		t.Fatalf("types %+v", got)
	}
}

func TestFlashbackScanHeapPageUsesInfomaskNotInfomask2(t *testing.T) {
	page := make([]byte, flashbackHeapPageSize)
	pdLower := uint16(flashbackPageHeaderSize + 4)
	pdUpper := uint16(flashbackHeapPageSize - 40)
	binary.LittleEndian.PutUint16(page[12:14], pdLower)
	binary.LittleEndian.PutUint16(page[14:16], pdUpper)
	binary.LittleEndian.PutUint16(page[16:18], uint16(flashbackHeapPageSize))
	lpLen := uint16(32)
	item := uint32(pdUpper) | (uint32(flashbackLPNormal) << 15) | (uint32(lpLen) << 17)
	binary.LittleEndian.PutUint32(page[flashbackPageHeaderSize:flashbackPageHeaderSize+4], item)
	page[pdUpper+22] = 24
	binary.LittleEndian.PutUint16(page[pdUpper+18:pdUpper+20], 21) // infomask2 奇数，bit0=1，不能当成 HASNULL
	binary.LittleEndian.PutUint16(page[pdUpper+20:pdUpper+22], 0)
	binary.LittleEndian.PutUint32(page[pdUpper+24:pdUpper+28], 7)
	rel := flashbackBuildRel([]flashbackColumn{flashbackCol("id", "int4", 4, "i")})
	rows := flashbackScanHeapPage(page, 0, rel, true)
	if len(rows) != 1 || rows[0].Values["id"] != "7" {
		t.Fatalf("values=%v", rows)
	}
}

func TestFlashbackPDUFillOldFromHeap(t *testing.T) {
	dir := t.TempDir()
	page := make([]byte, flashbackHeapPageSize)
	pdLower := uint16(flashbackPageHeaderSize + 4)
	pdUpper := uint16(flashbackHeapPageSize - 64)
	binary.LittleEndian.PutUint16(page[12:14], pdLower)
	binary.LittleEndian.PutUint16(page[14:16], pdUpper)
	binary.LittleEndian.PutUint16(page[16:18], uint16(flashbackHeapPageSize))
	lpLen := uint16(48)
	item := uint32(pdUpper) | (uint32(flashbackLPNormal) << 15) | (uint32(lpLen) << 17)
	binary.LittleEndian.PutUint32(page[flashbackPageHeaderSize:flashbackPageHeaderSize+4], item)
	page[pdUpper+22] = 24
	binary.LittleEndian.PutUint32(page[pdUpper+4:pdUpper+8], 9) // xmax，删行仍在页上
	binary.LittleEndian.PutUint32(page[pdUpper+24:pdUpper+28], 1)
	name := []byte("alice")
	page[pdUpper+28] = byte((len(name)+1)<<1 | 1)
	copy(page[pdUpper+29:], name)
	rel := flashbackBuildRel([]flashbackColumn{
		flashbackCol("id", "int4", 4, "i"),
		flashbackCol("cname", "text", -1, "i"),
	})
	rel.RelNode = 1234
	path := flashbackHeapRelationPath(dir, 5, 1234)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, page, 0o600); err != nil {
		t.Fatal(err)
	}
	dict := &flashbackDictionary{
		DBOID:  5,
		Wanted: map[string]*flashbackRelation{"public.tbl_flashback": rel},
	}
	// 在线闪回 REPLICA IDENTITY DEFAULT 只带主键，Old 里没有 cname
	ch := flashbackChange{Schema: "public", Table: "tbl_flashback", Op: "DELETE", Block: 0, Offnum: 1, Old: map[string]string{"id": "1"}}
	if !flashbackValuesIncomplete(rel, ch.Old) {
		t.Fatal("missing cname should be incomplete")
	}
	flashbackPDUFillOldFromHeap(dir, dict, &ch)
	if ch.Old["id"] != "1" || ch.Old["cname"] != "alice" {
		t.Fatalf("old=%v", ch.Old)
	}
	ch2 := flashbackChange{Schema: "public", Table: "tbl_flashback", Op: "DELETE", Block: 0, Offnum: 1, Old: map[string]string{"id": "1"}}
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	flashbackMergeOldFromPage(rel, &ch2, page, 0)
	if ch2.Old["cname"] != "alice" {
		t.Fatalf("merge page old=%v", ch2.Old)
	}
}

func TestFlashbackHeapRelFile(t *testing.T) {
	p, off := flashbackHeapRelFile(5, 1234, 0)
	if p != "base/5/1234" || off != 0 {
		t.Fatalf("%s %d", p, off)
	}
	p, off = flashbackHeapRelFile(5, 1234, 131072)
	if p != "base/5/1234.1" || off != 0 {
		t.Fatalf("seg1 %s %d", p, off)
	}
}

func TestFlashbackScanHeapPageInt(t *testing.T) {
	page := make([]byte, flashbackHeapPageSize)
	pdLower := uint16(flashbackPageHeaderSize + 4)
	pdUpper := uint16(flashbackHeapPageSize - 40)
	binary.LittleEndian.PutUint16(page[12:14], pdLower)
	binary.LittleEndian.PutUint16(page[14:16], pdUpper)
	binary.LittleEndian.PutUint16(page[16:18], uint16(flashbackHeapPageSize))
	lpLen := uint16(32)
	item := uint32(pdUpper) | (uint32(flashbackLPNormal) << 15) | (uint32(lpLen) << 17)
	binary.LittleEndian.PutUint32(page[flashbackPageHeaderSize:flashbackPageHeaderSize+4], item)
	hoff := byte(24)
	page[pdUpper+22] = hoff
	binary.LittleEndian.PutUint32(page[pdUpper+24:pdUpper+28], 42)
	rel := flashbackBuildRel([]flashbackColumn{flashbackCol("id", "int4", 4, "i")})
	rows := flashbackScanHeapPage(page, 0, rel, true)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Values["id"] != "42" {
		t.Fatalf("values=%v", rows[0].Values)
	}
}

func TestFlashbackReadPGVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	major, ver, err := flashbackReadPGVersion(dir)
	if err != nil || major != 16 || ver != "16" {
		t.Fatalf("major=%d ver=%s err=%v", major, ver, err)
	}
}

func TestFlashbackParseArchiveDest(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"", ""},
		{"(disabled)", ""},
		{"cp %p /data/wal_archive/%f", "/data/wal_archive"},
		{`test ! -f /archive/%f && cp %p /archive/%f`, "/archive"},
		{`gzip < %p > /mnt/nfs/wal/%f.gz`, "/mnt/nfs/wal"},
		{"wal_ship.sh %p %f", ""},
	}
	for _, tc := range cases {
		if got := flashbackParseArchiveDest(tc.cmd); got != tc.want {
			t.Fatalf("cmd=%q got=%q want=%q", tc.cmd, got, tc.want)
		}
	}
}

func TestFlashbackLocalPGWAL(t *testing.T) {
	dir := t.TempDir()
	if got := flashbackLocalPGWAL(dir); got != "" {
		t.Fatalf("missing pg_wal should be empty, got %s", got)
	}
	wal := filepath.Join(dir, "pg_wal")
	if err := os.Mkdir(wal, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := flashbackLocalPGWAL(dir); got != wal {
		t.Fatalf("got=%s want=%s", got, wal)
	}
	if got := flashbackSuggestWALDir(dir); got != wal {
		t.Fatalf("suggest=%s", got)
	}
}

func TestFlashbackPDUStagingName(t *testing.T) {
	if got := flashbackPDUStagingName(""); got != "local" {
		t.Fatalf("empty=%s", got)
	}
	if got := flashbackPDUStagingName("pg-lab"); got != "pg-lab" {
		t.Fatalf("id=%s", got)
	}
	if strings.Contains(flashbackPDUStagingName("a/b"), "/") {
		t.Fatal("slash should be stripped")
	}
}

func TestFlashbackPDUWorkStamp(t *testing.T) {
	ts := time.Date(2026, 9, 3, 14, 50, 12, 345*int(time.Millisecond), time.Local)
	got := flashbackPDUWorkStamp(ts)
	if got != "20260903145012.345" {
		t.Fatalf("stamp=%s", got)
	}
}

func TestFlashbackPDUWorkDirs(t *testing.T) {
	ctx := context.Background()
	t.Setenv("FLASHBACK_OFFLINE_ROOT", "/tmp/db-flashback/offline")
	ts := time.Date(2026, 9, 3, 14, 50, 12, 345*int(time.Millisecond), time.Local)
	root, pg, wal := flashbackPDUWorkDirs(ctx, "pg-lab", ts)
	want := "/tmp/db-flashback/offline/pg-lab/20260903145012.345"
	if root != want || pg != filepath.Join(want, "pgdata") || wal != filepath.Join(want, "pg_wal") {
		t.Fatalf("root=%s pg=%s wal=%s", root, pg, wal)
	}
}

func TestFlashbackPDUWantedKeys(t *testing.T) {
	got := flashbackPDUWantedKeys([]string{"tbl_flashback", "public.orders", "shop"})
	if !flashbackPDUWantedHit(got, "public", "tbl_flashback") {
		t.Fatal("bare name should match public.tbl_flashback")
	}
	if flashbackPDUWantedHit(got, "other", "tbl_flashback") {
		t.Fatal("bare name should not match other schema")
	}
	if !flashbackPDUWantedHit(got, "public", "orders") || flashbackPDUWantedHit(got, "public", "other") {
		t.Fatal("schema.table should be exact")
	}
	if !flashbackPDUWantedHit(got, "shop", "any") {
		t.Fatal("schema-only should match all tables in shop")
	}
}

func TestFlashbackPDUDictMissError(t *testing.T) {
	err := flashbackPDUDictMissError(&flashbackOfflineCatalog{DBName: "postgres"}, []string{"public.t"}, false)
	if err == nil || !strings.Contains(err.Error(), "库 postgres") || !strings.Contains(err.Error(), "单表") {
		t.Fatalf("%v", err)
	}
	err = flashbackPDUDictMissError(&flashbackOfflineCatalog{DBName: "app"}, nil, true)
	if err == nil || !strings.Contains(err.Error(), "整库") {
		t.Fatalf("%v", err)
	}
}

func TestFlashbackSelectPDUWALIgnoresMtimeAfterEnd(t *testing.T) {
	now := time.Now()
	files := []flashbackWALFile{
		{Name: "000000010000000000000001", Size: 16, Modification: now.Add(time.Hour), Source: "archive"},
		{Name: "000000010000000000000002", Size: 16, Modification: now.Add(2 * time.Hour), Source: "archive"},
	}
	end := now.Add(-time.Minute)
	online, _, _ := flashbackSelectWAL(files, now.Add(-time.Hour), end, 1<<30, "")
	if len(online) != 0 {
		t.Fatalf("online archive filter should drop future mtime, got %d", len(online))
	}
	picked, total, _ := flashbackSelectPDUWAL(files, 1<<30)
	if len(picked) != 2 || total != 32 {
		t.Fatalf("PDU should keep all copied segments, got %d total=%d", len(picked), total)
	}
}

func TestFlashbackPDUNoWALError(t *testing.T) {
	from := time.Date(2026, 9, 3, 15, 26, 0, 0, time.Local)
	to := time.Date(2026, 9, 3, 15, 47, 0, 0, time.Local)
	err := flashbackPDUNoWALError("/tmp/wal", nil, from, to)
	if err == nil || !strings.Contains(err.Error(), "没有可解析的段") {
		t.Fatalf("%v", err)
	}
	err = flashbackPDUNoWALError("/tmp/wal", []flashbackWALFile{{Name: "000000010000000000000001", Modification: to}}, from, to)
	if err == nil || !strings.Contains(err.Error(), "未选中 WAL 段") || !strings.Contains(err.Error(), "目录 1 个") {
		t.Fatalf("%v", err)
	}
}

func TestFlashbackListWorkWALKeepsArchive(t *testing.T) {
	dir := t.TempDir()
	name := "000000010000000000000001"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := flashbackListWorkWAL(dir)
	if err != nil || len(files) != 1 || files[0].Source != "archive" {
		t.Fatalf("files=%+v err=%v", files, err)
	}
}

func TestFlashbackMaterializeWALNilDBCopiesLocal(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	name := "00000001000000000000000A"
	if err := os.WriteFile(filepath.Join(src, name), []byte("hello-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, n, err := flashbackMaterializeWAL(context.Background(), nil, dst, src, flashbackWALFile{
		Name: name, Size: 9, Source: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || n != 9 || string(raw) != "hello-wal" {
		t.Fatalf("path=%s n=%d raw=%q err=%v", path, n, raw, err)
	}
}

func TestFlashbackFormatLocalTime(t *testing.T) {
	if flashbackFormatLocalTime(time.Time{}) != "—" {
		t.Fatal("zero")
	}
	ts := time.Date(2026, 9, 3, 8, 15, 23, 0, time.UTC)
	got := flashbackFormatLocalTime(ts)
	if !strings.Contains(got, "2026-09-03 16:15:23+08:00") {
		t.Fatalf("got=%s", got)
	}
}

func TestFlashbackParseTimeShanghai(t *testing.T) {
	got, err := flashbackParseTime("2026-09-03 16:42:41")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 3, 8, 42, 41, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got=%s want=%s", got.UTC(), want)
	}
	z, err := flashbackParseTime("2026-09-03T08:49:00Z")
	if err != nil || !z.Equal(time.Date(2026, 9, 3, 8, 49, 0, 0, time.UTC)) {
		t.Fatalf("rfc3339 z=%s err=%v", z, err)
	}
}

func TestFlashbackOnlineWALCoverage(t *testing.T) {
	target, err := flashbackParseTime("2026-09-03 16:42:41")
	if err != nil {
		t.Fatal(err)
	}
	end, err := flashbackParseTime("2026-09-03 16:48:50")
	if err != nil {
		t.Fatal(err)
	}
	latest := time.Date(2026, 9, 3, 8, 49, 0, 0, time.UTC)
	ok, msg := flashbackOnlineWALCoverage(target, end, latest, true)
	if !ok || strings.Contains(msg, "Z") && !strings.Contains(msg, "+08:00") {
		t.Fatalf("covered=%v msg=%s", ok, msg)
	}
	if !strings.Contains(msg, "16:42:41+08:00") || !strings.Contains(msg, "16:49:00+08:00") {
		t.Fatalf("msg=%s", msg)
	}
	if strings.Contains(msg, "2026-03-03") {
		t.Fatalf("must not use recycled start: %s", msg)
	}
	future := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ok, msg = flashbackOnlineWALCoverage(future, future.Add(time.Hour), latest, false)
	if ok || !strings.Contains(msg, "晚于 WAL 覆盖") {
		t.Fatalf("future covered=%v msg=%s", ok, msg)
	}
}

func TestFlashbackPDUCoverage(t *testing.T) {
	target := time.Date(2026, 9, 3, 15, 15, 0, 0, time.Local)
	end := time.Date(2026, 9, 3, 15, 33, 0, 0, time.Local)
	latest := time.Date(2026, 9, 3, 16, 15, 23, 0, time.Local)
	st, msg, covered := flashbackPDUCoverage(target, end, latest)
	if st != flashbackCheckPassed || !covered || !strings.Contains(msg, "回收槽位") {
		t.Fatalf("st=%s covered=%v msg=%s", st, covered, msg)
	}
	old := time.Date(2026, 9, 3, 14, 0, 0, 0, time.Local)
	st, msg, covered = flashbackPDUCoverage(target, end, old)
	if st != flashbackCheckWarning || covered || !strings.Contains(msg, "早于任务起始") {
		t.Fatalf("st=%s covered=%v msg=%s", st, covered, msg)
	}
	dir := flashbackPDUWALDirMessage(19, 19*16<<20, latest)
	if !strings.Contains(dir, "19 个 WAL 段") || strings.Contains(dir, "2026-03-02") {
		t.Fatalf("%s", dir)
	}
}

func TestFlashbackPDUCopyFileKeepsMtime(t *testing.T) {
	srcDir := t.TempDir()
	dst := filepath.Join(t.TempDir(), "wal", "000000010000000000000001")
	src := filepath.Join(srcDir, "000000010000000000000001")
	if err := os.WriteFile(src, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 9, 3, 15, 30, 0, 0, time.Local)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	if err := flashbackPDUCopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().UTC().Truncate(time.Second) != old.UTC().Truncate(time.Second) {
		t.Fatalf("mtime=%s want=%s", info.ModTime(), old)
	}
}

func TestFlashbackPDUCopyDirSkipWAL(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "pg_wal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pg_wal", "0001"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flashbackPDUCopyDir(src, dst, map[string]bool{"pg_wal": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "PG_VERSION")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "pg_wal")); err == nil {
		t.Fatal("pg_wal should be skipped")
	}
}

func TestFlashbackPDUHostIsLocal(t *testing.T) {
	if !flashbackPDUHostIsLocal("127.0.0.1") || !flashbackPDUHostIsLocal("localhost") {
		t.Fatal("loopback should be local")
	}
	if flashbackPDUHostIsLocal("10.100.112.17") {
		t.Fatal("lab host should be remote")
	}
}

func TestFlashbackPDUSSHValid(t *testing.T) {
	ok := flashbackPDUSSH{Host: "10.100.112.17", User: "postgres", Port: 22}
	if err := ok.valid(); err != nil {
		t.Fatal(err)
	}
	bad := flashbackPDUSSH{Host: "10.100.112.17;rm", User: "postgres", Port: 22}
	if err := bad.valid(); err == nil {
		t.Fatal("expected reject")
	}
	if err := flashbackPDURemotePathOK("/home/postgres/data"); err != nil {
		t.Fatal(err)
	}
	if err := flashbackPDURemotePathOK("../etc"); err == nil {
		t.Fatal("expected reject relative")
	}
}

func TestFlashbackPDUSafeJoin(t *testing.T) {
	work := t.TempDir()
	if _, err := flashbackPDUSafeJoin(work, "../etc/passwd"); err == nil {
		t.Fatal("expected reject")
	}
	want := filepath.Join(work, "restore", "a.csv")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := flashbackPDUSafeJoin(work, "restore/a.csv")
	if err != nil || got != want {
		t.Fatalf("got=%s err=%v", got, err)
	}
}
