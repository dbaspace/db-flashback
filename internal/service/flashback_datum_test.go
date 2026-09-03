package service

import (
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlashbackDecodeFixedTypes(t *testing.T) {
	col := func(name string, typlen int, align string) flashbackColumn {
		return flashbackColumn{Name: "c", TypeName: name, Typlen: typlen, Typalign: align}
	}
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, 42)
	s, n, ok := flashbackReadDatum(b, col("int4", 4, "i"))
	if !ok || n != 4 || s != "42" {
		t.Fatalf("int4 got %q n=%d ok=%v", s, n, ok)
	}
	s, _, ok = flashbackReadDatum([]byte{1}, col("bool", 1, "c"))
	if !ok || s != "t" {
		t.Fatalf("bool got %q", s)
	}
	u := make([]byte, 16)
	u[0], u[1], u[2], u[3] = 0x01, 0x23, 0x45, 0x67
	u[4], u[5], u[6], u[7] = 0x89, 0xab, 0xcd, 0xef
	u[8], u[9], u[10], u[11] = 0x10, 0x32, 0x54, 0x76
	u[12], u[13], u[14], u[15] = 0x98, 0xba, 0xdc, 0xfe
	s, n, ok = flashbackReadDatum(u, col("uuid", 16, "c"))
	if !ok || n != 16 || s != "01234567-89ab-cdef-1032-547698badcfe" {
		t.Fatalf("uuid got %q", s)
	}
	d := make([]byte, 4)
	binary.LittleEndian.PutUint32(d, 0) // 2000-01-01
	s, _, ok = flashbackReadDatum(d, col("date", 4, "i"))
	if !ok || s != "2000-01-01" {
		t.Fatalf("date got %q", s)
	}
	ts := make([]byte, 8)
	s, _, ok = flashbackReadDatum(ts, col("timestamptz", 8, "d"))
	if !ok || !strings.HasPrefix(s, "2000-01-01 00:00:00") {
		t.Fatalf("timestamptz got %q", s)
	}
}

func TestFlashbackDecodeTextVarlena(t *testing.T) {
	col := flashbackColumn{Name: "c", TypeName: "text", Typlen: -1, Typalign: "i"}
	raw := []byte{7, 'h', 'i'} // short header len=3
	s, n, ok := flashbackReadDatum(raw, col)
	if !ok || n != 3 || s != "hi" {
		t.Fatalf("text got %q n=%d ok=%v", s, n, ok)
	}
}

func TestFlashbackDecodeNumeric(t *testing.T) {
	// 123.45 → weight=0 dscale=2 digits=[123, 4500]
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[0:2], 2) // dscale=2, positive long
	binary.LittleEndian.PutUint16(b[2:4], 0) // weight
	binary.LittleEndian.PutUint16(b[4:6], 123)
	binary.LittleEndian.PutUint16(b[6:8], 4500)
	s, ok := flashbackDecodeNumeric(b)
	if !ok || s != "123.45" {
		t.Fatalf("numeric got %q ok=%v", s, ok)
	}
}

func TestFlashbackDecodeByteaAndInet(t *testing.T) {
	col := flashbackColumn{Name: "c", TypeName: "bytea", Typlen: -1, Typalign: "i"}
	payload := []byte{0xde, 0xad}
	raw := []byte{byte((3 << 1) | 1), 0xde, 0xad}
	s, _, ok := flashbackReadDatum(raw, col)
	if !ok || s != "\\xdead" {
		t.Fatalf("bytea got %q", s)
	}
	inet := []byte{2, 32, 0, 4, 10, 1, 2, 3} // send format 10.1.2.3/32
	s, ok = flashbackDecodeInet(inet, false)
	if !ok || s != "10.1.2.3" {
		t.Fatalf("inet send got %q", s)
	}
	heap := make([]byte, 18)
	heap[0], heap[1] = 2, 32
	heap[2], heap[3], heap[4], heap[5] = 10, 0, 0, 1
	s, ok = flashbackDecodeInet(heap, false)
	if !ok || s != "10.0.0.1" {
		t.Fatalf("inet heap got %q", s)
	}
	compact := []byte{2, 32, 192, 168, 0, 2}
	s, ok = flashbackDecodeInet(compact, false)
	if !ok || s != "192.168.0.2" {
		t.Fatalf("inet compact got %q", s)
	}
	_ = payload
}

func TestFlashbackDecodeJSONBNumeric(t *testing.T) {
	// 自测 WAL 真实布局：{"k": 1}。key 后 INTALIGN，varlena 头 8<<2=0x20 + short numeric 1。
	// 旧实现把 pad/varlena 头当数字解，失败后再写成 0。
	jb1 := []byte{0x01, 0x00, 0x00, 0x20, 0x01, 0x00, 0x00, 0x80, 0x0b, 0x00, 0x00, 0x10, 0x6b, 0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00, 0x00, 0x80, 0x01, 0x00}
	s, ok := flashbackDecodeJSONB(jb1)
	if !ok || s != `{"k": 1}` {
		t.Fatalf("jsonb {k:1} got %q ok=%v", s, ok)
	}

	// {"k": 123.45}：short numeric dscale=2 weight=0 digits=[123,4500]，varlena 10<<2=0x28。
	jbDec := []byte{0x01, 0x00, 0x00, 0x20, 0x01, 0x00, 0x00, 0x80, 0x0d, 0x00, 0x00, 0x10, 0x6b, 0x00, 0x00, 0x00, 0x28, 0x00, 0x00, 0x00, 0x00, 0x81, 0x7b, 0x00, 0x94, 0x11}
	s, ok = flashbackDecodeJSONB(jbDec)
	if !ok || s != `{"k": 123.45}` {
		t.Fatalf("jsonb {k:123.45} got %q ok=%v", s, ok)
	}

	// 真实 0 仍应解成 0，而不是解码失败。
	jb0 := []byte{0x01, 0x00, 0x00, 0x20, 0x01, 0x00, 0x00, 0x80, 0x09, 0x00, 0x00, 0x10, 0x6b, 0x00, 0x00, 0x00, 0x18, 0x00, 0x00, 0x00, 0x00, 0x80}
	s, ok = flashbackDecodeJSONB(jb0)
	if !ok || s != `{"k": 0}` {
		t.Fatalf("jsonb {k:0} got %q ok=%v", s, ok)
	}

	// 失败不得再回退成 "0"。
	if s, ok := flashbackDecodeJSONBNumeric([]byte{0x00, 0x00, 0x20, 0x00}, 0, 4); ok {
		t.Fatalf("garbage numeric should fail, got %q", s)
	}
}

func TestFlashbackDecodeMoneyAndInterval(t *testing.T) {
	col := flashbackColumn{Name: "c", TypeName: "money", Typlen: 8, Typalign: "d"}
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(12345)) // 123.45
	s, _, ok := flashbackReadDatum(b, col)
	if !ok || s != "123.45" {
		t.Fatalf("money got %q", s)
	}
	iv := make([]byte, 16)
	binary.LittleEndian.PutUint64(iv[0:8], 3661000000) // 1h 1m 1s
	binary.LittleEndian.PutUint32(iv[8:12], 2)
	binary.LittleEndian.PutUint32(iv[12:16], 13)
	s, ok = flashbackFormatInterval(int64(binary.LittleEndian.Uint64(iv[0:8])), 2, 13), true
	if !strings.Contains(s, "1 years") || !strings.Contains(s, "1 mons") || !strings.Contains(s, "2 days") {
		t.Fatalf("interval got %q", s)
	}
}

func TestFlashbackDroppedColumnHexAndAlign(t *testing.T) {
	// WalMiner：DROP COLUMN 之后，DDL 前该列按 encode(..., 'hex') 解析，且必须占位否则后续列错位。
	dropped := flashbackColumn{
		Name: "........pg.dropped.2........", TypeName: "dropped",
		Typlen: -1, Typalign: "i", Dropped: true,
	}
	raw := []byte{byte((3 << 1) | 1), 0xad, 0x97}
	s, n, ok := flashbackReadDatum(raw, dropped)
	if !ok || n != 3 || !strings.HasPrefix(s, `\RAW:encode('\xad97'`) {
		t.Fatalf("dropped hex got %q n=%d ok=%v", s, n, ok)
	}
	if got := flashbackQuoteLiteral(s); !strings.HasPrefix(got, "encode(") || strings.Contains(got, `'encode`) {
		t.Fatalf("RAW encode should not be re-quoted: %s", got)
	}

	custom := flashbackColumn{Name: "c", TypeName: "mytype", Typlen: -1, Typalign: "i", TypType: "b"}
	s, _, ok = flashbackReadDatum(raw, custom)
	if !ok || !strings.Contains(s, `\RAW:encode(`) {
		t.Fatalf("custom type should be hex encode, got %q", s)
	}

	// int4 + dropped varlena + int4
	tuple := make([]byte, 36)
	tuple[22] = 24
	binary.LittleEndian.PutUint32(tuple[24:28], 7)
	tuple[28] = byte((3 << 1) | 1)
	tuple[29], tuple[30] = 0xad, 0x97
	// int4 按 i 对齐：31 起需 pad 到 32
	binary.LittleEndian.PutUint32(tuple[32:36], 9)
	rel := &flashbackRelation{
		Columns: []flashbackColumn{
			{Name: "id", TypeName: "int4", Typlen: 4, Typalign: "i"},
			dropped,
			{Name: "age", TypeName: "int4", Typlen: 4, Typalign: "i"},
		},
	}
	got := flashbackDecodeHeapTuple(rel, tuple)
	if got["id"] != "7" || got["age"] != "9" {
		t.Fatalf("alignment after dropped col: %#v", got)
	}
	if _, ok := got[dropped.Name]; ok {
		t.Fatalf("dropped col should not enter undo map: %#v", got)
	}
}

func TestFlashbackTypeSupported(t *testing.T) {
	st, _ := flashbackTypeSupported(flashbackColumn{TypeName: "jsonb"})
	if st != "supported" {
		t.Fatalf("jsonb %s", st)
	}
	st, _ = flashbackTypeSupported(flashbackColumn{TypeName: "mystored", TypType: "b"})
	if st != "unsupported" {
		t.Fatalf("custom type should be unsupported, got %s", st)
	}
}

func TestFlashbackWALProbeOrder(t *testing.T) {
	files := []flashbackWALFile{
		{Name: "000000010000000000000001"},
		{Name: "000000010000000000000002"},
		{Name: "000000010000000000000003"},
	}
	got := flashbackWALProbeOrder(files, "000000010000000000000002")
	if len(got) != 3 || got[0] != "000000010000000000000002" || got[1] != "000000010000000000000003" {
		t.Fatalf("got %v", got)
	}
}

func TestFlashbackFilterCurrentTimeline(t *testing.T) {
	files := []flashbackWALFile{
		{Name: "000000010000000000000001", Source: "live"},
		{Name: "000000020000000000000001", Source: "live"},
		{Name: "000000010000000000000002", Source: "live"},
	}
	got := flashbackFilterCurrentTimeline(files, "000000010000000000000002")
	if len(got) != 2 {
		t.Fatalf("want TLI 1 files, got %d", len(got))
	}
	for _, f := range got {
		if !strings.HasPrefix(f.Name, "00000001") {
			t.Fatalf("unexpected %s", f.Name)
		}
	}
}

func TestFlashbackWALContinuityGaps(t *testing.T) {
	files := []flashbackWALFile{
		{Name: "000000010000000000000001"},
		{Name: "000000010000000000000003"},
	}
	gaps := flashbackWALContinuityGaps(files)
	if len(gaps) != 1 {
		t.Fatalf("want 1 gap, got %v", gaps)
	}
	ok := flashbackWALContinuityGaps([]flashbackWALFile{
		{Name: "000000010000000000000001"},
		{Name: "000000010000000000000002"},
	})
	if len(ok) != 0 {
		t.Fatalf("consecutive should have no gap: %v", ok)
	}
}

func TestFlashbackIsXLogSwitch(t *testing.T) {
	rec := make([]byte, 24)
	rec[16] = xlogSwitch
	rec[17] = rmXLog
	if !flashbackIsXLogSwitch(rec) {
		t.Fatal("expected switch")
	}
	rec[17] = rmHeap
	if flashbackIsXLogSwitch(rec) {
		t.Fatal("heap is not switch")
	}
}

func TestFlashbackIsXLogCheckpoint(t *testing.T) {
	rec := make([]byte, 24)
	rec[16] = xlogCheckpointOnline
	rec[17] = rmXLog
	if !flashbackIsXLogCheckpoint(rec) {
		t.Fatal("expected online checkpoint")
	}
	rec[16] = xlogCheckpointShutdown
	if !flashbackIsXLogCheckpoint(rec) {
		t.Fatal("expected shutdown checkpoint")
	}
	rec[16] = xlogSwitch
	if flashbackIsXLogCheckpoint(rec) {
		t.Fatal("switch is not checkpoint")
	}
}

func TestFlashbackToastPointerIsNull(t *testing.T) {
	col := flashbackColumn{Name: "c", TypeName: "text", Typlen: -1, Typalign: "i"}
	b := make([]byte, toastPointerMinLen)
	b[0] = varattIs1BE
	s, n, ok := flashbackReadDatum(b, col)
	if !ok || n != toastPointerMinLen || s != `\N` {
		t.Fatalf("toast pointer got %q n=%d ok=%v", s, n, ok)
	}
}

func TestFlashbackToastCacheResolve(t *testing.T) {
	cache := newFlashbackToastCache()
	cache.put(99, 7, 0, []byte("hello-toast"))
	p := flashbackToastPtr{RawSize: 15, ValueID: 7, ToastRel: 99}
	s, ok := flashbackResolveToast(p, cache, flashbackColumn{Name: "c", TypeName: "text", Typlen: -1})
	if !ok || s != "hello-toast" {
		t.Fatalf("got %q ok=%v", s, ok)
	}
}

func TestFlashbackDictionarySnapshotRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flashbackDictFileName)
	dict := &flashbackDictionary{
		DBOID: 11, DBName: "db",
		Wanted: map[string]*flashbackRelation{
			"public.t": {
				Schema: "public", Name: "t", OID: 1, RelNode: 2, ToastOID: 9, ToastNode: 8,
				PKCols: []string{"id"},
				Columns: []flashbackColumn{
					{Name: "id", AttNum: 1, TypeName: "int4", Typlen: 4, Typalign: "i", IsPK: true, Default: "1"},
				},
			},
		},
	}
	flashbackBindDictionary(dict)
	if err := flashbackSaveDictionaryFile(path, dict); err != nil {
		t.Fatal(err)
	}
	got, err := flashbackLoadDictionaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rel := got.Wanted["public.t"]
	if rel == nil || rel.OID != 1 || rel.ToastOID != 9 || rel.Columns[0].Default != "1" {
		t.Fatalf("%+v", rel)
	}
	if got.toastOwner(8) == nil {
		t.Fatal("toast owner by filenode")
	}
}

func TestFlashbackShortVarlenaUnaligned(t *testing.T) {
	// int4 + bool + text + bytea：text/bytea 短头可从非 4 字节对齐处开始。
	raw := make([]byte, 24+4+1+6+3)
	raw[22] = 24
	binary.LittleEndian.PutUint32(raw[24:28], 1)
	raw[28] = 1 // bool t
	raw[29] = byte((6 << 1) | 1)
	copy(raw[30:35], []byte("hello"))
	raw[35] = byte((3 << 1) | 1)
	raw[36], raw[37] = 0xde, 0xad
	rel := &flashbackRelation{
		Columns: []flashbackColumn{
			{Name: "id", TypeName: "int4", Typlen: 4, Typalign: "i"},
			{Name: "c_bool", TypeName: "bool", Typlen: 1, Typalign: "c"},
			{Name: "c_text", TypeName: "text", Typlen: -1, Typalign: "i"},
			{Name: "c_bytea", TypeName: "bytea", Typlen: -1, Typalign: "i"},
		},
	}
	got := flashbackDecodeHeapTuple(rel, raw)
	if got["id"] != "1" || got["c_bool"] != "t" || got["c_text"] != "hello" || got["c_bytea"] != "\\xdead" {
		t.Fatalf("got %#v", got)
	}
}
