package service

import (
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
)

// varatt_1b_e + varatt_external：header(1)+tag(1)+rawsize(4)+extinfo(4)+valueid(4)+toastrelid(4)=18。
const (
	toastCompressPGLZ  = 0
	toastCompressLZ4   = 1
	varlenaExtSizeBits = 30
)

type flashbackToastPtr struct {
	RawSize  int32
	ExtSize  uint32
	Compress uint32
	ValueID  uint32
	ToastRel uint32
}

type flashbackToastCache struct {
	mu     sync.Mutex
	chunks map[uint64]map[int][]byte // (toastOID<<32|valueID) -> seq -> data
}

func newFlashbackToastCache() *flashbackToastCache {
	return &flashbackToastCache{chunks: map[uint64]map[int][]byte{}}
}

func flashbackToastKey(toastOID, valueID uint32) uint64 {
	return uint64(toastOID)<<32 | uint64(valueID)
}

func (c *flashbackToastCache) put(toastOID, valueID uint32, seq int, data []byte) {
	if c == nil || valueID == 0 || seq < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chunks == nil {
		c.chunks = map[uint64]map[int][]byte{}
	}
	k := flashbackToastKey(toastOID, valueID)
	if c.chunks[k] == nil {
		c.chunks[k] = map[int][]byte{}
	}
	c.chunks[k][seq] = append([]byte(nil), data...)
}

func (c *flashbackToastCache) get(toastOID, valueID uint32) ([]byte, bool) {
	if c == nil || valueID == 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := c.chunks[flashbackToastKey(toastOID, valueID)]
	if len(parts) == 0 {
		return nil, false
	}
	max := -1
	for seq := range parts {
		if seq > max {
			max = seq
		}
	}
	var out []byte
	for i := 0; i <= max; i++ {
		b, ok := parts[i]
		if !ok {
			return nil, false
		}
		out = append(out, b...)
	}
	return out, true
}

func flashbackParseToastPtr(b []byte) (p flashbackToastPtr, n int, ok bool) {
	if len(b) < toastPointerMinLen || b[0] != varattIs1BE {
		return flashbackToastPtr{}, 0, false
	}
	p.RawSize = int32(binary.LittleEndian.Uint32(b[2:6]))
	ext := binary.LittleEndian.Uint32(b[6:10])
	p.ExtSize = ext & ((1 << varlenaExtSizeBits) - 1)
	p.Compress = ext >> varlenaExtSizeBits
	p.ValueID = binary.LittleEndian.Uint32(b[10:14])
	p.ToastRel = binary.LittleEndian.Uint32(b[14:18])
	return p, toastPointerMinLen, p.ValueID != 0
}

func flashbackResolveToast(p flashbackToastPtr, toast *flashbackToastCache, col flashbackColumn) (string, bool) {
	if toast == nil {
		return `\N`, true
	}
	raw, ok := toast.get(p.ToastRel, p.ValueID)
	if !ok {
		return `\N`, true
	}
	payload, ok := flashbackToastPayload(p, raw)
	if !ok {
		return `\N`, true
	}
	s, ok := flashbackDecodeVarlena(payload, col)
	return s, ok
}

func flashbackToastPayload(p flashbackToastPtr, raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	want := int(p.RawSize)
	if want >= 4 {
		want -= 4 // VARHDRSZ
	}
	if p.Compress != 0 || (p.ExtSize > 0 && int(p.ExtSize) < int(p.RawSize)-4) {
		dest := make([]byte, want)
		var n int
		var err error
		switch p.Compress {
		case toastCompressLZ4:
			n, err = flashbackDecompressFPW(bkpImageLZ4, raw, dest)
		default:
			n, err = flashbackPGLZDecompress(raw, dest)
		}
		if err != nil || n != len(dest) {
			return raw, len(raw) > 0
		}
		return dest, true
	}
	// 未压缩：chunk 拼出完整 varlena（含 4 字节头）或裸 payload。
	if len(raw) >= 4 && int(binary.LittleEndian.Uint32(raw[:4])>>2) == len(raw) {
		return raw[4:], true
	}
	return raw, true
}

func flashbackToastChunkRel() *flashbackRelation {
	return &flashbackRelation{
		Columns: []flashbackColumn{
			{Name: "chunk_id", AttNum: 1, TypeName: "oid", Typlen: 4, Typalign: "i"},
			{Name: "chunk_seq", AttNum: 2, TypeName: "int4", Typlen: 4, Typalign: "i"},
			{Name: "chunk_data", AttNum: 3, TypeName: "bytea", Typlen: -1, Typalign: "i"},
		},
	}
}

func flashbackIngestToastRecord(dict *flashbackDictionary, owner *flashbackRelation, rmid, op, info byte, blk *xlogBlockData, main []byte) {
	if dict == nil || dict.Toast == nil || owner == nil {
		return
	}
	toastOID := owner.ToastOID
	if toastOID == 0 {
		toastOID = owner.ToastNode
	}
	rel := flashbackToastChunkRel()
	var rows []map[string]string
	if rmid == rmHeap2 && op == xlogHeap2MultiInsert {
		ch := flashbackDecodeMultiInsert(rel, 0, blk, main, info, func(c flashbackChange) []flashbackChange {
			return []flashbackChange{c}
		})
		for _, c := range ch {
			rows = append(rows, c.New)
		}
	} else if rmid == rmHeap && op == xlogHeapInsert {
		if vals := flashbackDecodeXLogTuple(rel, blk.data); len(vals) > 0 {
			rows = append(rows, vals)
		}
	} else if rmid == rmHeap && (op == xlogHeapUpdate || op == xlogHeapHotUpdate) {
		newRaw := blk.data
		if len(main) >= flashbackSizeOfHeapUpdate {
			newRaw = flashbackSkipUpdatePrefixSuffix(blk.data, main[7])
		}
		if vals := flashbackDecodeXLogTuple(rel, newRaw); len(vals) > 0 {
			rows = append(rows, vals)
		}
	}
	for _, vals := range rows {
		flashbackToastPutDecoded(dict.Toast, toastOID, vals)
	}
}

func flashbackToastPutDecoded(cache *flashbackToastCache, toastOID uint32, vals map[string]string) {
	if cache == nil || len(vals) == 0 {
		return
	}
	id64, err := strconv.ParseUint(vals["chunk_id"], 10, 32)
	if err != nil || id64 == 0 {
		return
	}
	seq64, err := strconv.ParseInt(vals["chunk_seq"], 10, 32)
	if err != nil || seq64 < 0 {
		return
	}
	raw, ok := flashbackByteaFromDecoded(vals["chunk_data"])
	if !ok {
		return
	}
	cache.put(toastOID, uint32(id64), int(seq64), raw)
}

func flashbackByteaFromDecoded(s string) ([]byte, bool) {
	if s == "" || s == `\N` {
		return nil, false
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `\RAW:`) {
		s = strings.TrimPrefix(s, `\RAW:`)
	}
	if strings.HasPrefix(s, `\x`) || strings.HasPrefix(s, `\\x`) {
		s = strings.TrimPrefix(strings.TrimPrefix(s, `\\x`), `\x`)
		b, err := hex.DecodeString(s)
		return b, err == nil
	}
	return []byte(s), true
}
