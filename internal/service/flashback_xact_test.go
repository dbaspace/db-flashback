package service

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFlashbackPGTimestampRoundtrip(t *testing.T) {
	want := time.Date(2026, 8, 26, 1, 50, 0, 0, time.UTC)
	got := flashbackPGTimestamp(flashbackTimeToPGTimestamp(want))
	if !got.Equal(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestFlashbackParseXactCommit_plainAndSubxids(t *testing.T) {
	ts := time.Date(2026, 8, 26, 17, 50, 0, 0, time.UTC)
	us := flashbackTimeToPGTimestamp(ts)
	plain := make([]byte, 8)
	binary.LittleEndian.PutUint64(plain, uint64(us))
	cmt := flashbackParseXactCommit(xlogXactCommit, plain, false)
	if !cmt.Time.Equal(ts) || len(cmt.SubXIDs) != 0 {
		t.Fatalf("plain: %+v", cmt)
	}

	body := make([]byte, 8+4+8+4+8)
	binary.LittleEndian.PutUint64(body[0:8], uint64(us))
	binary.LittleEndian.PutUint32(body[8:12], xactXinfoHasDBInfo|xactXinfoHasSubxacts)
	binary.LittleEndian.PutUint32(body[20:24], 2)
	binary.LittleEndian.PutUint32(body[24:28], 101)
	binary.LittleEndian.PutUint32(body[28:32], 102)
	cmt = flashbackParseXactCommit(xlogXactCommit|xlogXactHasInfo, body, false)
	if !cmt.Time.Equal(ts) || len(cmt.SubXIDs) != 2 || cmt.SubXIDs[0] != 101 || cmt.SubXIDs[1] != 102 {
		t.Fatalf("subxids: %+v", cmt)
	}

	prep := make([]byte, 4+8)
	binary.LittleEndian.PutUint32(prep[0:4], 9)
	binary.LittleEndian.PutUint64(prep[4:12], uint64(us))
	cmt = flashbackParseXactCommit(xlogXactCommitPrepared, prep, true)
	if !cmt.Time.Equal(ts) {
		t.Fatalf("prepared: %s", cmt.Time)
	}
}

func TestFlashbackFilterCommitTime(t *testing.T) {
	from := time.Date(2026, 8, 26, 1, 50, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	in := []flashbackChange{
		{XID: 1, TS: from.Add(-time.Minute), Op: "DELETE"},
		{XID: 2, TS: from.Add(time.Minute), Op: "DELETE"},
		{XID: 3, TS: to.Add(time.Minute), Op: "DELETE"},
		{XID: 4, Op: "DELETE"},
	}
	got := flashbackFilterCommitTime(in, from, to)
	if len(got) != 1 || got[0].XID != 2 {
		t.Fatalf("got %+v", got)
	}
}

func flashbackTestXactMainWrapped(main []byte) []byte {
	if len(main) < 256 {
		return append([]byte{xlrBlockIDDataShort, byte(len(main))}, main...)
	}
	hdr := make([]byte, 5)
	hdr[0] = xlrBlockIDDataLong
	binary.LittleEndian.PutUint32(hdr[1:5], uint32(len(main)))
	return append(hdr, main...)
}

func flashbackTestCommitRecord(xid uint32, ts time.Time) []byte {
	main := make([]byte, 8)
	binary.LittleEndian.PutUint64(main, uint64(flashbackTimeToPGTimestamp(ts)))
	body := flashbackTestXactMainWrapped(main)
	tot := flashbackSizeOfXLogRecord + len(body)
	rec := make([]byte, tot)
	binary.LittleEndian.PutUint32(rec[0:4], uint32(tot))
	binary.LittleEndian.PutUint32(rec[4:8], xid)
	rec[16] = xlogXactCommit
	rec[17] = rmXact
	copy(rec[flashbackSizeOfXLogRecord:], body)
	return rec
}

func flashbackTestAbortRecord(xid uint32) []byte {
	main := make([]byte, 8)
	body := flashbackTestXactMainWrapped(main)
	tot := flashbackSizeOfXLogRecord + len(body)
	rec := make([]byte, tot)
	binary.LittleEndian.PutUint32(rec[0:4], uint32(tot))
	binary.LittleEndian.PutUint32(rec[4:8], xid)
	rec[16] = xlogXactAbort
	rec[17] = rmXact
	copy(rec[flashbackSizeOfXLogRecord:], body)
	return rec
}

func TestFlashbackXactMainDataWrapped(t *testing.T) {
	ts := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	rec := flashbackTestCommitRecord(42, ts)
	main := flashbackXactMainData(rec)
	cmt := flashbackParseXactCommit(xlogXactCommit, main, false)
	if !cmt.Time.Equal(ts) {
		t.Fatalf("wrapped commit time %s want %s main=%x", cmt.Time, ts, main)
	}
}

func flashbackTestWALPage(recs ...[]byte) []byte {
	page := make([]byte, flashbackXLogPageSize)
	binary.LittleEndian.PutUint16(page[0:2], 0xD110)
	binary.LittleEndian.PutUint16(page[2:4], xlpLongHeader)
	off := 40
	for _, rec := range recs {
		n := flashbackMaxAlign(len(rec))
		if off+n > len(page) {
			panic("test wal page overflow")
		}
		copy(page[off:], rec)
		off += n
	}
	return page
}

func flashbackTestDict24740() *flashbackDictionary {
	rel := &flashbackRelation{
		Schema: "public", Name: "tbl_test", RelNode: 24740, DBOID: 24622,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	return &flashbackDictionary{
		DBOID:     24622,
		ByRelNode: map[uint32]*flashbackRelation{24740: rel},
		Wanted:    map[string]*flashbackRelation{"public.tbl_test": rel},
	}
}

func TestFlashbackParseCommitTimeWindowAndAbort(t *testing.T) {
	dict := flashbackTestDict24740()
	inWin := time.Date(2026, 8, 26, 1, 55, 0, 0, time.UTC)
	outWin := time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC)
	page := flashbackTestWALPage(
		flashbackTestInsertRecord(11, 24622, 24740, 1, "old", nil),
		flashbackTestCommitRecord(11, outWin),
		flashbackTestInsertRecord(12, 24622, 24740, 2, "keep", nil),
		flashbackTestCommitRecord(12, inWin),
		flashbackTestInsertRecord(13, 24622, 24740, 3, "abort", nil),
		flashbackTestAbortRecord(13),
	)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000000010000000000000001"), page, 0o600); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 8, 26, 1, 50, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	ch, _, err := flashbackParseWALDirOpts(dir, dict, 24622, flashbackParseOpts{TimeFrom: from, TimeTo: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 1 || ch[0].New["id"] != "2" || ch[0].XID != 12 {
		t.Fatalf("want only committed-in-window xid=12, got %+v", ch)
	}
	if ch[0].TS.IsZero() || !ch[0].TS.Equal(inWin) {
		t.Fatalf("commit time: %s", ch[0].TS)
	}
}

func TestFlashbackParseAbortDropsUncommitted(t *testing.T) {
	dict := flashbackTestDict24740()
	page := flashbackTestWALPage(
		flashbackTestInsertRecord(21, 24622, 24740, 9, "gone", nil),
		flashbackTestAbortRecord(21),
	)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000000010000000000000001"), page, 0o600); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 8, 26, 1, 50, 0, 0, time.UTC)
	ch, _, err := flashbackParseWALDirOpts(dir, dict, 24622, flashbackParseOpts{TimeFrom: from, TimeTo: from.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatalf("aborted txn must not emit, got %+v", ch)
	}
}

func TestFlashbackTxnBufSubxidFlush(t *testing.T) {
	buf := flashbackNewTxnBuf()
	buf.add(flashbackChange{XID: 5, Op: "DELETE", Table: "t"})
	buf.add(flashbackChange{XID: 6, Op: "INSERT", Table: "t"})
	ts := time.Date(2026, 8, 26, 2, 0, 0, 0, time.UTC)
	out := buf.flush(flashbackXactCommit{Time: ts, SubXIDs: []uint32{6}}, 5)
	if len(out) != 2 {
		t.Fatalf("want parent+sub, got %d", len(out))
	}
	for _, c := range out {
		if !c.TS.Equal(ts) {
			t.Fatalf("ts %s", c.TS)
		}
	}
	if len(buf.dumpAll()) != 0 {
		t.Fatal("pending should be empty")
	}
}
