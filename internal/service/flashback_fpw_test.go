package service

import (
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func flashbackTestPGLZLiterals(raw []byte) []byte {
	var out []byte
	for off := 0; off < len(raw); {
		n := len(raw) - off
		if n > 8 {
			n = 8
		}
		out = append(out, 0)
		out = append(out, raw[off:off+n]...)
		off += n
	}
	return out
}

func TestFlashbackPGLZDecompressMatch(t *testing.T) {
	// 3 字面量 + 回指 3 字节（对齐 PG pglz control/match）。
	src := []byte{0x08, 'A', 'B', 'C', 0x00, 0x03}
	dest := make([]byte, 6)
	n, err := flashbackPGLZDecompress(src, dest)
	if err != nil || n != 6 || string(dest) != "ABCABC" {
		t.Fatalf("n=%d err=%v dest=%q", n, err, dest)
	}
}

func TestFlashbackPGLZDecompressLiterals(t *testing.T) {
	raw := []byte("hello-pglz!!")
	src := flashbackTestPGLZLiterals(raw)
	dest := make([]byte, len(raw))
	n, err := flashbackPGLZDecompress(src, dest)
	if err != nil || n != len(raw) || string(dest) != string(raw) {
		t.Fatalf("n=%d err=%v dest=%q", n, err, dest)
	}
}

func TestFlashbackRebuildFPWCompressedLZ4(t *testing.T) {
	page := make([]byte, flashbackXLogPageSize)
	for i := range page {
		page[i] = byte(i)
	}
	packed := page // no hole
	dst := make([]byte, lz4.CompressBlockBound(len(packed)))
	n, err := lz4.CompressBlock(packed, dst, nil)
	if err != nil || n == 0 {
		t.Fatalf("lz4 compress: %v n=%d", err, n)
	}
	b := &xlogBlockData{
		image: dst[:n], holeOff: 0, holeLen: 0, bimgInfo: bkpImageLZ4, compressed: true,
	}
	got := flashbackRebuildFPW(b)
	if len(got) != flashbackXLogPageSize {
		t.Fatalf("page len %d", len(got))
	}
	for i := 0; i < 64; i++ {
		if got[i] != page[i] {
			t.Fatalf("byte %d: %d != %d", i, got[i], page[i])
		}
	}
}

func TestFlashbackRebuildFPWCompressedZSTD(t *testing.T) {
	page := make([]byte, flashbackXLogPageSize)
	for i := range page {
		page[i] = byte(i)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	comp := enc.EncodeAll(page, nil)
	b := &xlogBlockData{
		image: comp, holeOff: 0, holeLen: 0, bimgInfo: bkpImageZSTD, compressed: true,
	}
	got := flashbackRebuildFPW(b)
	if len(got) != flashbackXLogPageSize {
		t.Fatalf("page len %d", len(got))
	}
	for i := 0; i < 64; i++ {
		if got[i] != page[i] {
			t.Fatalf("byte %d: %d != %d", i, got[i], page[i])
		}
	}
}

func TestFlashbackRebuildFPWCompressedPGLZ(t *testing.T) {
	packed := make([]byte, 64)
	for i := range packed {
		packed[i] = byte(i + 1)
	}
	b := &xlogBlockData{
		image: flashbackTestPGLZLiterals(packed), holeOff: 64, holeLen: uint16(flashbackXLogPageSize - 64),
		bimgInfo: bkpImagePGLZ, compressed: true,
	}
	got := flashbackRebuildFPW(b)
	if len(got) != flashbackXLogPageSize {
		t.Fatalf("page len %d", len(got))
	}
	for i := 0; i < 64; i++ {
		if got[i] != packed[i] {
			t.Fatalf("byte %d", i)
		}
	}
}

func TestFlashbackUpdateMissingOldDropped(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "t", RelNode: 10, DBOID: 1,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
		},
	}
	dict := &flashbackDictionary{DBOID: 1, ByRelNode: map[uint32]*flashbackRelation{10: rel}}
	tuple := flashbackTestXLogTuple(3, "x")
	var body []byte
	body = append(body, 0, bkpHasData)
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	body = append(body, dl...)
	put32 := func(v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		body = append(body, b...)
	}
	put32(1663)
	put32(1)
	put32(10)
	put32(0)
	body = append(body, xlrBlockIDDataShort, byte(flashbackSizeOfHeapUpdate))
	body = append(body, tuple...)
	upd := make([]byte, flashbackSizeOfHeapUpdate)
	binary.LittleEndian.PutUint16(upd[4:6], 1)
	upd[7] = 0
	body = append(body, upd...)
	rec := make([]byte, flashbackSizeOfXLogRecord+len(body))
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(rec)))
	rec[16] = xlogHeapUpdate
	rec[17] = rmHeap
	copy(rec[flashbackSizeOfXLogRecord:], body)
	st := &flashbackParseStats{}
	ch := flashbackDecodeHeapRecord(rec, dict, 1, nil, st)
	if len(ch) != 0 {
		t.Fatalf("missing old must not emit fake update: %+v", ch)
	}
	if st.UpdateNoOld == 0 {
		t.Fatal("expected UpdateNoOld")
	}
}

func TestFlashbackTopXIDRemap(t *testing.T) {
	rel := &flashbackRelation{
		Schema: "public", Name: "t", RelNode: 10, DBOID: 1,
		PKCols: []string{"id"},
		Columns: []flashbackColumn{
			{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true},
			{Name: "cname", AttNum: 2, TypeName: "text", Typlen: -1, Typalign: "i"},
		},
	}
	dict := &flashbackDictionary{DBOID: 1, ByRelNode: map[uint32]*flashbackRelation{10: rel}}
	tuple := flashbackTestXLogTuple(4, "y")
	var body []byte
	body = append(body, 0, bkpHasData)
	dl := make([]byte, 2)
	binary.LittleEndian.PutUint16(dl, uint16(len(tuple)))
	body = append(body, dl...)
	put32 := func(v uint32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, v)
		body = append(body, b...)
	}
	put32(1663)
	put32(1)
	put32(10)
	put32(0)
	body = append(body, xlrBlockIDTopXID)
	put32(99)
	body = append(body, xlrBlockIDDataShort, byte(flashbackSizeOfHeapInsert))
	body = append(body, tuple...)
	body = append(body, 1, 0, 0)
	rec := make([]byte, flashbackSizeOfXLogRecord+len(body))
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(rec)))
	binary.LittleEndian.PutUint32(rec[4:8], 7)
	rec[16] = xlogHeapInsert
	rec[17] = rmHeap
	copy(rec[flashbackSizeOfXLogRecord:], body)
	ch := flashbackDecodeHeapRecord(rec, dict, 1, nil, nil)
	if len(ch) != 1 || ch[0].XID != 99 {
		t.Fatalf("top xid want 99, got %+v", ch)
	}
}

func TestFlashbackDecodeHeapTruncate(t *testing.T) {
	rel := &flashbackRelation{Schema: "public", Name: "orders", RelNode: 88, DBOID: 1}
	dict := &flashbackDictionary{DBOID: 1, ByRelNode: map[uint32]*flashbackRelation{88: rel}}
	main := make([]byte, 20)
	binary.LittleEndian.PutUint32(main[0:4], 1)
	binary.LittleEndian.PutUint32(main[4:8], 1)
	binary.LittleEndian.PutUint32(main[16:20], 88)
	body := []byte{xlrBlockIDDataShort, byte(len(main))}
	body = append(body, main...)
	rec := make([]byte, flashbackSizeOfXLogRecord+len(body))
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(rec)))
	binary.LittleEndian.PutUint32(rec[4:8], 5)
	rec[16] = xlogHeapTruncate
	rec[17] = rmHeap
	copy(rec[flashbackSizeOfXLogRecord:], body)
	ch := flashbackDecodeHeapRecord(rec, dict, 1, nil, nil)
	if len(ch) != 1 || ch[0].Op != "TRUNCATE" || ch[0].Table != "orders" {
		t.Fatalf("got %+v", ch)
	}
	undo, risk := flashbackUndoSQL(ch[0])
	if undo == "" || risk == "" || !flashbackWantOp(map[string]struct{}{"DDL": {}}, "TRUNCATE") {
		t.Fatalf("truncate sql undo=%q risk=%q", undo, risk)
	}
	redo, _ := flashbackRedoSQL(ch[0])
	if redo != `TRUNCATE TABLE "public"."orders";` {
		t.Fatalf("redo %q", redo)
	}
}
