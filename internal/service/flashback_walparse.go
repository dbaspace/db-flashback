package service

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	flashbackXLogPageSize     = 8192
	flashbackSizeOfXLogRecord = 24
	flashbackSizeOfHeapHeader = 5
	flashbackSizeOfHeapInsert = 3
	flashbackSizeOfHeapDelete = 8
	flashbackSizeOfHeapUpdate = 14
	flashbackSizeofHeapTuple  = 23

	xlpLongHeader      = 0x0002
	xlpFirstContRecord = 0x0001

	xlrBlockIDDataShort = 255
	xlrBlockIDDataLong  = 254
	xlrBlockIDOrigin    = 253
	xlrBlockIDTopXID    = 252
	xlrMaxBlockID       = 32

	bkpHasImage = 0x10
	bkpHasData  = 0x20
	bkpSameRel  = 0x80

	bkpImageHasHole = 0x01
	bkpImagePGLZ    = 0x04
	bkpImageLZ4     = 0x08
	bkpImageZSTD    = 0x10

	rmXLog  = 0
	rmXact  = 1
	rmHeap2 = 9
	rmHeap  = 10

	xlogXactCommit = 0x00
	xlogXactAbort  = 0x20

	xlogHeapInsert    = 0x00
	xlogHeapDelete    = 0x10
	xlogHeapUpdate    = 0x20
	xlogHeapTruncate  = 0x30
	xlogHeapHotUpdate = 0x40
	xlogHeapOpMask    = 0x70
	xlogHeapInitPage  = 0x80

	// HEAP2 操作码以官方 heapam_xlog.h 为准（14–18 的 MULTI_INSERT 都是 0x50）。
	xlogHeap2MultiInsert   = 0x50
	xlogCheckpointShutdown = 0x00
	xlogCheckpointOnline   = 0x10
	xlogSwitch             = 0x40
	xlogFPIForHint         = 0xA0
	xlogFPI                = 0xA1
	xlrInfoMask            = 0x0F

	xlhInsertContainsNewTuple = 1 << 3

	xlhDeleteContainsOldTuple = 1 << 1
	xlhDeleteContainsOldKey   = 1 << 2
	xlhUpdateContainsOldTuple = 1 << 2
	xlhUpdateContainsOldKey   = 1 << 3
	xlhUpdateContainsNewTuple = 1 << 4
	xlhUpdatePrefixFromOld    = 1 << 5
	xlhUpdateSuffixFromOld    = 1 << 6

	heapHasNull = 0x0001
	lpNormal    = 1
)

var flashbackPageMagics = map[uint16]string{
	0xD097: "12",
	0xD106: "13",
	0xD10D: "14",
	0xD110: "15",
	0xD111: "15+",
	0xD113: "16",
	0xD116: "17",
	0xD118: "18",
	0xD121: "19",
}

func flashbackLooksLikeWALMagic(magic uint16) bool {
	if _, ok := flashbackPageMagics[magic]; ok {
		return true
	}
	// 小版本可能微调 magic，只要落在 WAL 常用区间就继续解析。
	return magic >= 0xD090 && magic <= 0xD200
}

type flashbackParseStats struct {
	Records, Heap, Matched, Decoded        int
	Inserts, Deletes, Updates              int
	FPW                                    int
	DeleteNoOld, UpdateNoOld, FPWMiss      int
	Pages, MagicSkip, SplitFail            int
	Truncates, TruncateSeen, FPWDecompress int
	Magic                                  uint16
	WantedDB, WantedRel                    uint32
	SeenRels                               string
	ChangeTrunc                            bool
	MultiInserts, MultiInsertRows          int
	Checkpoints                            int
}

func flashbackMaxAlign(n int) int {
	if n < 0 {
		return 0
	}
	return (n + 7) &^ 7
}

func (s flashbackParseStats) String() string {
	msg := fmt.Sprintf("页 %d，magic=0x%04X，跳过 %d，记录 %d，Heap %d，命中目标表 %d，解码 %d（INSERT %d / DELETE %d / UPDATE %d），FPW %d 页",
		s.Pages, s.Magic, s.MagicSkip, s.Records, s.Heap, s.Matched, s.Decoded, s.Inserts, s.Deletes, s.Updates, s.FPW)
	if s.SplitFail > 0 {
		msg += fmt.Sprintf("，拆包失败 %d", s.SplitFail)
	}
	if s.FPWMiss > 0 {
		msg += fmt.Sprintf("，FPW 未找到 %d", s.FPWMiss)
	}
	if s.WantedRel != 0 {
		msg += fmt.Sprintf("，字典 dboid=%d relfilenode=%d", s.WantedDB, s.WantedRel)
	}
	if s.SeenRels != "" {
		msg += "，WAL Heap rel=" + s.SeenRels
	}
	if s.ChangeTrunc {
		msg += "，变更条数已截断"
	}
	if s.MultiInserts > 0 {
		msg += fmt.Sprintf("，MULTI_INSERT/COPY %d 次/%d 行", s.MultiInserts, s.MultiInsertRows)
	}
	msg += fmt.Sprintf("，HEAP_TRUNCATE 记录 %d / 命中 %d", s.TruncateSeen, s.Truncates)
	if s.FPWDecompress > 0 {
		msg += fmt.Sprintf("，压缩 FPW 解压 %d", s.FPWDecompress)
	}
	if s.UpdateNoOld > 0 {
		msg += fmt.Sprintf("，UPDATE 缺旧行 %d", s.UpdateNoOld)
	}
	if s.DeleteNoOld > 0 {
		msg += fmt.Sprintf("，DELETE 缺旧行 %d", s.DeleteNoOld)
	}
	if s.Checkpoints > 0 {
		msg += fmt.Sprintf("，CHECKPOINT %d", s.Checkpoints)
	}
	return msg
}

type flashbackParseOpts struct {
	MaxChanges  int
	DeleteAfter bool
	MaxFPWPages int
	TimeFrom    time.Time
	TimeTo      time.Time
}

type flashbackFPWCache struct {
	max  int
	keys []uint64
	data map[uint64][]byte
}

func flashbackNewFPWCache(max int) *flashbackFPWCache {
	if max <= 0 {
		max = flashbackDefaultFPWPages
	}
	return &flashbackFPWCache{max: max, data: map[uint64][]byte{}}
}

func (c *flashbackFPWCache) Get(k uint64) []byte {
	if c == nil {
		return nil
	}
	return c.data[k]
}

func (c *flashbackFPWCache) Set(k uint64, page []byte) {
	if c == nil || len(page) == 0 {
		return
	}
	if _, ok := c.data[k]; ok {
		c.data[k] = page
		return
	}
	if c.max > 0 && len(c.data) >= c.max && len(c.keys) > 0 {
		old := c.keys[0]
		c.keys = c.keys[1:]
		delete(c.data, old)
	}
	c.data[k] = page
	c.keys = append(c.keys, k)
}

func (c *flashbackFPWCache) Len() int {
	if c == nil {
		return 0
	}
	return len(c.data)
}

func (s *flashbackParseStats) noteRel(db, rel uint32) {
	if s == nil || rel == 0 {
		return
	}
	item := fmt.Sprintf("%d/%d", db, rel)
	if s.SeenRels == "" {
		s.SeenRels = item
		return
	}
	if strings.Contains(s.SeenRels, item) {
		return
	}
	if strings.Count(s.SeenRels, ",") >= 11 {
		return
	}
	s.SeenRels += "," + item
}

func flashbackParseWALDir(dir string, dict *flashbackDictionary, dboid uint32) ([]flashbackChange, flashbackParseStats, error) {
	return flashbackParseWALDirOpts(dir, dict, dboid, flashbackParseOpts{})
}

func flashbackParseWALDirOpts(dir string, dict *flashbackDictionary, dboid uint32, opts flashbackParseOpts) ([]flashbackChange, flashbackParseStats, error) {
	var out []flashbackChange
	st, err := flashbackWalkWALDir(dir, dict, dboid, opts, func(ch flashbackChange) bool {
		out = append(out, ch)
		return opts.MaxChanges <= 0 || len(out) < opts.MaxChanges
	})
	return out, st, err
}

func flashbackWalkWALDir(dir string, dict *flashbackDictionary, dboid uint32, opts flashbackParseOpts, emit func(flashbackChange) bool) (flashbackParseStats, error) {
	var st flashbackParseStats
	ents, err := os.ReadDir(dir)
	if err != nil {
		return st, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && flashbackIsWALSegName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if dict != nil {
		st.WantedDB = dict.DBOID
		for _, rel := range dict.Wanted {
			if rel.RelNode != 0 {
				st.WantedRel = rel.RelNode
				break
			}
		}
	}
	sort.Strings(names)
	p := &flashbackWALParser{
		dict: dict, dboid: dboid, fpw: flashbackNewFPWCache(opts.MaxFPWPages), st: &st,
		maxChanges: opts.MaxChanges, timeFrom: opts.TimeFrom, timeTo: opts.TimeTo,
		txn: flashbackNewTxnBuf(),
	}
	for _, name := range names {
		if p.maxChanges > 0 && p.emitted >= p.maxChanges {
			st.ChangeTrunc = true
			break
		}
		if p.pastEnd {
			break
		}
		path := filepath.Join(dir, name)
		ch, err := p.feedFile(path)
		if opts.DeleteAfter {
			_ = os.Remove(path)
		}
		if err != nil {
			return st, fmt.Errorf("%s: %w", name, err)
		}
		for _, c := range ch {
			if emit != nil && !emit(c) {
				st.ChangeTrunc = true
				return st, nil
			}
		}
	}
	flashbackFinishParser(p, opts, emit, &st)
	return st, nil
}

func flashbackFinishParser(p *flashbackWALParser, opts flashbackParseOpts, emit func(flashbackChange) bool, st *flashbackParseStats) {
	if p == nil {
		return
	}
	if opts.hasTimeWindow() {
		return
	}
	for _, c := range p.flushPending() {
		if emit != nil && !emit(c) {
			if st != nil {
				st.ChangeTrunc = true
			}
			return
		}
	}
	if p.dict != nil && p.dict.Catalog != nil {
		for _, c := range p.dict.Catalog.flushAll() {
			if emit != nil && !emit(c) {
				if st != nil {
					st.ChangeTrunc = true
				}
				return
			}
		}
	}
}

func flashbackCountRelNodeInDir(dir string, relNode uint32) int {
	if relNode == 0 {
		return 0
	}
	var needle [4]byte
	binary.LittleEndian.PutUint32(needle[:], relNode)
	return flashbackCountNeedleInDir(dir, needle[:])
}

func flashbackCountRelPairInDir(dir string, dboid, relNode uint32) int {
	if dboid == 0 || relNode == 0 {
		return 0
	}
	var needle [8]byte
	binary.LittleEndian.PutUint32(needle[0:4], dboid)
	binary.LittleEndian.PutUint32(needle[4:8], relNode)
	return flashbackCountNeedleInDir(dir, needle[:])
}

func flashbackCountNeedleInDir(dir string, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	buf := make([]byte, 64*1024)
	for _, e := range ents {
		if e.IsDir() || !flashbackIsWALSegName(e.Name()) {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		n += flashbackCountNeedleInReader(f, needle, buf)
		_ = f.Close()
	}
	return n
}

func flashbackCountNeedleInReader(r io.Reader, needle []byte, buf []byte) int {
	if len(needle) == 0 {
		return 0
	}
	if len(buf) < 4096 {
		buf = make([]byte, 64*1024)
	}
	n := 0
	overlap := len(needle) - 1
	var carry []byte
	for {
		got, err := r.Read(buf)
		if got > 0 {
			chunk := buf[:got]
			if len(carry) > 0 {
				chunk = append(carry, chunk...)
			}
			for i := 0; i+len(needle) <= len(chunk); i++ {
				if bytes.Equal(chunk[i:i+len(needle)], needle) {
					n++
				}
			}
			if overlap > 0 && len(chunk) >= overlap {
				carry = append(carry[:0], chunk[len(chunk)-overlap:]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	return n
}

func flashbackCopyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if _, err := flashbackCopyFileStream(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

type flashbackWALParser struct {
	dict       *flashbackDictionary
	dboid      uint32
	fpw        *flashbackFPWCache
	st         *flashbackParseStats
	cont       []byte
	contNeed   int
	padSkip    int
	maxChanges int
	emitted    int
	txn        *flashbackTxnBuf
	timeFrom   time.Time
	timeTo     time.Time
	pastEnd    bool
}

func flashbackParseWALFile(path string, dict *flashbackDictionary, dboid uint32, fpw *flashbackFPWCache, st *flashbackParseStats) ([]flashbackChange, error) {
	if fpw == nil {
		fpw = flashbackNewFPWCache(0)
	}
	p := &flashbackWALParser{dict: dict, dboid: dboid, fpw: fpw, st: st}
	return p.feedFile(path)
}

func (p *flashbackWALParser) take(ch []flashbackChange) []flashbackChange {
	if len(ch) == 0 {
		return nil
	}
	if p.maxChanges <= 0 {
		p.emitted += len(ch)
		return ch
	}
	left := p.maxChanges - p.emitted
	if left <= 0 {
		if p.st != nil {
			p.st.ChangeTrunc = true
		}
		return nil
	}
	if len(ch) > left {
		ch = ch[:left]
		if p.st != nil {
			p.st.ChangeTrunc = true
		}
	}
	p.emitted += len(ch)
	return ch
}

func (p *flashbackWALParser) emit(rec []byte) []flashbackChange {
	if p.st != nil {
		p.st.Records++
	}
	if p.pastEnd || len(rec) < flashbackSizeOfXLogRecord {
		return nil
	}
	if flashbackIsXLogCheckpoint(rec) && p.st != nil {
		p.st.Checkpoints++
	}
	rmid := rec[17]
	info := rec[16]
	xid := binary.LittleEndian.Uint32(rec[4:8])
	if p.txn == nil {
		p.txn = flashbackNewTxnBuf()
	}
	if rmid == rmXact {
		return p.handleXact(rec, xid, info)
	}
	for _, c := range flashbackDecodeHeapRecord(rec, p.dict, p.dboid, p.fpw, p.st) {
		p.txn.add(c)
	}
	return nil
}

func (p *flashbackWALParser) handleXact(rec []byte, xid uint32, info byte) []flashbackChange {
	op := info & xlogXactOpMask
	payload := flashbackXactMainData(rec)
	prepared := op == xlogXactCommitPrepared || op == xlogXactAbortPrepared
	switch op {
	case xlogXactCommit, xlogXactCommitPrepared:
		cmt := flashbackParseXactCommit(info, payload, prepared)
		ddl := flashbackDecodeHeapRecord(rec, p.dict, p.dboid, p.fpw, p.st)
		if p.dict != nil && p.dict.Catalog != nil {
			for _, id := range cmt.SubXIDs {
				ddl = append(ddl, p.dict.Catalog.flushXID(id)...)
			}
		}
		out := append(p.txn.flush(cmt, xid), ddl...)
		for i := range out {
			if out[i].TS.IsZero() {
				out[i].TS = cmt.Time
			}
		}
		if !p.timeTo.IsZero() && !cmt.Time.IsZero() && cmt.Time.After(p.timeTo) {
			p.pastEnd = true
		}
		return flashbackFilterCommitTime(out, p.timeFrom, p.timeTo)
	case xlogXactAbort, xlogXactAbortPrepared:
		cmt := flashbackParseXactCommit(info, payload, prepared)
		ids := append([]uint32{xid}, cmt.SubXIDs...)
		p.txn.discard(ids...)
		if p.dict != nil && p.dict.Catalog != nil {
			for _, id := range ids {
				p.dict.Catalog.discardXID(id)
			}
		}
		return nil
	default:
		return flashbackDecodeHeapRecord(rec, p.dict, p.dboid, p.fpw, p.st)
	}
}

func (p *flashbackWALParser) flushPending() []flashbackChange {
	if p == nil || p.txn == nil {
		return nil
	}
	return p.take(p.txn.dumpAll())
}

func (p *flashbackWALParser) feedFile(path string) ([]flashbackChange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	page := make([]byte, flashbackXLogPageSize)
	var out []flashbackChange
	for {
		if p.pastEnd {
			return out, nil
		}
		if p.maxChanges > 0 && p.emitted >= p.maxChanges {
			if p.st != nil {
				p.st.ChangeTrunc = true
			}
			return out, nil
		}
		if _, err := io.ReadFull(f, page); err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return out, err
		}
		if p.st != nil {
			p.st.Pages++
		}
		magic := binary.LittleEndian.Uint16(page[0:2])
		if p.st != nil && p.st.Magic == 0 && magic != 0 {
			p.st.Magic = magic
		}
		if !flashbackLooksLikeWALMagic(magic) {
			if p.st != nil {
				p.st.MagicSkip++
			}
			continue
		}
		info := binary.LittleEndian.Uint16(page[2:4])
		hdrLen := 24
		if info&xlpLongHeader != 0 {
			hdrLen = 40
		}
		if hdrLen > len(page) {
			continue
		}
		body := page[hdrLen:]
		if info&xlpFirstContRecord != 0 {
			rem := int(binary.LittleEndian.Uint32(page[16:20]))
			if rem < 0 {
				rem = 0
			}
			take := rem
			if take > len(body) {
				take = len(body)
			}
			if p.contNeed > 0 && take > 0 {
				p.cont = append(p.cont, body[:take]...)
				if len(p.cont) >= p.contNeed {
					rec := p.cont[:p.contNeed]
					out = append(out, p.take(p.emit(rec))...)
					p.cont = nil
					p.contNeed = 0
					if flashbackIsXLogSwitch(rec) {
						return out, nil
					}
				}
			}
			// xlogreader：下一条从 pageHeader + MAXALIGN(xlp_rem_len) 开始。
			if rem > len(body) {
				continue
			}
			skip := flashbackMaxAlign(rem)
			if skip > len(body) {
				p.padSkip = skip - len(body)
				continue
			}
			body = body[skip:]
			p.padSkip = 0
		} else {
			if p.padSkip > 0 {
				if p.padSkip >= len(body) {
					p.padSkip -= len(body)
					continue
				}
				body = body[p.padSkip:]
				p.padSkip = 0
			}
			if p.contNeed > 0 {
				p.cont = nil
				p.contNeed = 0
			}
		}
		for len(body) >= flashbackSizeOfXLogRecord {
			tot := int(binary.LittleEndian.Uint32(body[0:4]))
			if tot < flashbackSizeOfXLogRecord || tot > 1024*1024 {
				break
			}
			aligned := flashbackMaxAlign(tot)
			if tot > len(body) {
				p.cont = append([]byte(nil), body...)
				p.contNeed = tot
				break
			}
			out = append(out, p.take(p.emit(body[:tot]))...)
			if flashbackIsXLogSwitch(body[:tot]) {
				return out, nil
			}
			if aligned > len(body) {
				p.padSkip = aligned - len(body)
				break
			}
			body = body[aligned:]
		}
	}
	return out, nil
}

type xlogBlockData struct {
	relNode, dbNode, spcNode uint32
	blkno                    uint32
	data                     []byte
	image                    []byte
	holeOff, holeLen         uint16
	bimgInfo                 byte
	compressed               bool
}

func flashbackFPWKey(relNode, blkno uint32) uint64 {
	return uint64(relNode)<<32 | uint64(blkno)
}

func flashbackIsXLogSwitch(rec []byte) bool {
	if len(rec) < 18 {
		return false
	}
	return rec[17] == rmXLog && rec[16]&0xF0 == xlogSwitch
}

func flashbackIsXLogCheckpoint(rec []byte) bool {
	if len(rec) < 18 {
		return false
	}
	if rec[17] != rmXLog {
		return false
	}
	op := rec[16] & 0xF0
	return op == xlogCheckpointShutdown || op == xlogCheckpointOnline
}

// flashbackWALDataHasCheckpoint 扫 WAL 字节里是否出现 CHECKPOINT 记录（跨页 continuation 可能漏，仅作选段辅助）。
func flashbackWALDataHasCheckpoint(data []byte) bool {
	for off := 0; off+flashbackXLogPageSize <= len(data); off += flashbackXLogPageSize {
		page := data[off : off+flashbackXLogPageSize]
		magic := binary.LittleEndian.Uint16(page[0:2])
		if !flashbackLooksLikeWALMagic(magic) {
			continue
		}
		info := binary.LittleEndian.Uint16(page[2:4])
		hdrLen := 24
		if info&xlpLongHeader != 0 {
			hdrLen = 40
		}
		if hdrLen >= len(page) {
			continue
		}
		body := page[hdrLen:]
		if info&xlpFirstContRecord != 0 {
			rem := int(binary.LittleEndian.Uint32(page[16:20]))
			if rem < 0 {
				rem = 0
			}
			skip := flashbackMaxAlign(rem)
			if skip > len(body) {
				continue
			}
			body = body[skip:]
		}
		for len(body) >= flashbackSizeOfXLogRecord {
			tot := int(binary.LittleEndian.Uint32(body[0:4]))
			if tot < flashbackSizeOfXLogRecord || tot > len(body) {
				break
			}
			if flashbackIsXLogCheckpoint(body[:tot]) {
				return true
			}
			aligned := flashbackMaxAlign(tot)
			if aligned > len(body) {
				break
			}
			body = body[aligned:]
		}
	}
	return false
}

func flashbackWantFPW(dict *flashbackDictionary, relNode uint32) bool {
	if dict == nil {
		return false
	}
	if dict.ByRelNode[relNode] != nil {
		return true
	}
	if dict.toastOwner(relNode) != nil {
		return true
	}
	return dict.Catalog != nil && dict.Catalog.decoder(relNode) != nil
}

func flashbackCacheWantedFPW(dict *flashbackDictionary, blocks []xlogBlockData, fpw *flashbackFPWCache, st *flashbackParseStats) {
	if dict == nil || fpw == nil {
		return
	}
	for i := range blocks {
		b := &blocks[i]
		if !flashbackWantFPW(dict, b.relNode) {
			continue
		}
		page := flashbackRebuildFPW(b)
		if len(page) != flashbackXLogPageSize {
			continue
		}
		fpw.Set(flashbackFPWKey(b.relNode, b.blkno), page)
		if st != nil {
			st.FPW++
			if b.compressed {
				st.FPWDecompress++
			}
		}
	}
}

func flashbackDecodeHeapRecord(rec []byte, dict *flashbackDictionary, dboid uint32, fpw *flashbackFPWCache, st *flashbackParseStats) []flashbackChange {
	if len(rec) < flashbackSizeOfXLogRecord {
		return nil
	}
	xid := binary.LittleEndian.Uint32(rec[4:8])
	info := rec[16]
	rmid := rec[17]
	if rmid == rmXact {
		if dict == nil || dict.Catalog == nil {
			return nil
		}
		switch info & 0x70 {
		case xlogXactCommit, 0x30: // COMMIT / COMMIT_PREPARED
			return dict.Catalog.flushXID(xid)
		case xlogXactAbort, 0x40:
			dict.Catalog.discardXID(xid)
		}
		return nil
	}
	if rmid != rmHeap && rmid != rmHeap2 && rmid != rmXLog {
		return nil
	}
	blocks, main, topXID, ok := flashbackSplitXLogBody(rec[flashbackSizeOfXLogRecord:])
	// HEAP_TRUNCATE 通常不带 backup block，拆包失败时仍按 main 解码，避免漏记。
	if rmid == rmHeap && (info&xlogHeapOpMask) == xlogHeapTruncate {
		if st != nil {
			st.TruncateSeen++
			st.Matched++
		}
		if topXID != 0 {
			xid = topXID
		}
		if !ok || len(main) == 0 {
			main = rec[flashbackSizeOfXLogRecord:]
		}
		emit := func(ch flashbackChange) []flashbackChange {
			if st != nil {
				st.Decoded++
				if ch.Op == "TRUNCATE" {
					st.Truncates++
				}
			}
			return []flashbackChange{ch}
		}
		return flashbackDecodeHeapTruncate(xid, dboid, dict, main, emit)
	}
	if !ok {
		if st != nil && (rmid == rmHeap || rmid == rmHeap2) {
			st.SplitFail++
		}
		return nil
	}
	if topXID != 0 {
		xid = topXID
	}
	if rmid == rmHeap || rmid == rmHeap2 {
		if st != nil {
			st.Heap++
		}
		for i := range blocks {
			if st != nil {
				st.noteRel(blocks[i].dbNode, blocks[i].relNode)
			}
		}
	}
	flashbackCacheWantedFPW(dict, blocks, fpw, st)
	if rmid == rmXLog {
		return nil
	}
	if dict == nil {
		return nil
	}

	op := info & xlogHeapOpMask
	for i := range blocks {
		if owner := dict.toastOwner(blocks[i].relNode); owner != nil {
			flashbackIngestToastRecord(dict, owner, rmid, op, info, &blocks[i], main)
		}
	}
	lookupRel := func(b *xlogBlockData) *flashbackRelation {
		if r := dict.ByRelNode[b.relNode]; r != nil && !r.Missing {
			return r
		}
		if dict.Catalog != nil {
			if r := dict.Catalog.decoder(b.relNode); r != nil {
				return r
			}
			if r := dict.Catalog.userRelation(b.relNode); r != nil {
				return r
			}
		}
		return nil
	}
	var rel *flashbackRelation
	var blk *xlogBlockData
	for i := range blocks {
		b := &blocks[i]
		if dboid != 0 && b.dbNode != 0 && b.dbNode != dboid {
			continue
		}
		if r := lookupRel(b); r != nil {
			rel = r
			blk = b
			break
		}
	}
	if rel == nil {
		for i := range blocks {
			b := &blocks[i]
			if r := lookupRel(b); r != nil {
				rel = r
				blk = b
				break
			}
		}
	}
	emit := func(ch flashbackChange) []flashbackChange {
		if st != nil {
			st.Decoded++
			switch ch.Op {
			case "INSERT":
				st.Inserts++
			case "DELETE":
				st.Deletes++
			case "UPDATE":
				st.Updates++
			case "TRUNCATE":
				st.Truncates++
			}
		}
		return []flashbackChange{ch}
	}

	if rel == nil || blk == nil {
		return nil
	}
	if dict.Catalog != nil && dict.Catalog.decoder(blk.relNode) != nil {
		flashbackApplyCatalogHeap(dict.Catalog, rel, xid, rmid, op, info, blk, main, fpw, st)
		return nil
	}
	if st != nil {
		st.Matched++
	}

	if rmid == rmHeap2 {
		if op == xlogHeap2MultiInsert {
			ch := flashbackDecodeMultiInsert(rel, xid, blk, main, info, emit)
			if st != nil && len(ch) > 0 {
				st.MultiInserts++
				st.MultiInsertRows += len(ch)
			}
			return ch
		}
		return nil
	}

	switch op {
	case xlogHeapInsert:
		vals := flashbackDecodeXLogTuple(rel, blk.data)
		if len(vals) == 0 {
			return nil
		}
		offnum := uint16(0)
		if len(main) >= 2 {
			offnum = binary.LittleEndian.Uint16(main[0:2])
		}
		return emit(flashbackChange{
			XID: xid, Schema: rel.Schema, Table: rel.Name, Op: "INSERT",
			New: vals, PKCols: rel.PKCols, NoPK: len(rel.PKCols) == 0, RelNode: rel.RelNode,
			Block: blk.blkno, Offnum: offnum, NewBlock: blk.blkno, NewOff: offnum,
		})
	case xlogHeapDelete:
		offnum := uint16(0)
		flags := byte(0)
		if len(main) >= flashbackSizeOfHeapDelete {
			offnum = binary.LittleEndian.Uint16(main[4:6])
			flags = main[7]
		}
		var vals map[string]string
		if flags&xlhDeleteContainsOldTuple != 0 && len(main) > flashbackSizeOfHeapDelete {
			vals = flashbackDecodeXLogTuple(rel, main[flashbackSizeOfHeapDelete:])
		}
		if len(vals) == 0 {
			vals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(flashbackRebuildFPW(blk), offnum))
		}
		if len(vals) == 0 && fpw != nil && offnum > 0 {
			vals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(fpw.Get(flashbackFPWKey(rel.RelNode, blk.blkno)), offnum))
		}
		if len(vals) == 0 && flags&xlhDeleteContainsOldKey != 0 && len(main) > flashbackSizeOfHeapDelete {
			vals = flashbackDecodeXLogTuple(flashbackRelationPKOnly(rel), main[flashbackSizeOfHeapDelete:])
		}
		if len(vals) == 0 {
			if st != nil {
				st.DeleteNoOld++
				if fpw == nil || len(fpw.Get(flashbackFPWKey(rel.RelNode, blk.blkno))) == 0 {
					st.FPWMiss++
				}
			}
			return nil
		}
		return emit(flashbackChange{
			XID: xid, Schema: rel.Schema, Table: rel.Name, Op: "DELETE",
			Old: vals, PKCols: rel.PKCols, NoPK: len(rel.PKCols) == 0, RelNode: rel.RelNode,
			Block: blk.blkno, Offnum: offnum,
		})
	case xlogHeapUpdate, xlogHeapHotUpdate:
		oldVals := map[string]string{}
		newRaw := blk.data
		if len(main) >= flashbackSizeOfHeapUpdate {
			flags := main[7]
			newRaw = flashbackSkipUpdatePrefixSuffix(blk.data, flags)
		}
		newVals := flashbackDecodeXLogTuple(rel, newRaw)
		if len(main) >= flashbackSizeOfHeapUpdate {
			flags := main[7]
			oldOff := binary.LittleEndian.Uint16(main[4:6])
			if flags&xlhUpdateContainsOldTuple != 0 {
				oldVals = flashbackDecodeXLogTuple(rel, main[flashbackSizeOfHeapUpdate:])
			}
			if len(oldVals) == 0 {
				oldVals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(flashbackRebuildFPW(blk), oldOff))
			}
			if len(oldVals) == 0 && fpw != nil {
				oldVals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(fpw.Get(flashbackFPWKey(rel.RelNode, blk.blkno)), oldOff))
			}
			if len(oldVals) == 0 && flags&xlhUpdateContainsOldKey != 0 {
				oldVals = flashbackDecodeXLogTuple(flashbackRelationPKOnly(rel), main[flashbackSizeOfHeapUpdate:])
			}
			if flags&xlhUpdateContainsNewTuple != 0 && len(newVals) == 0 {
				newVals = flashbackDecodeXLogTuple(rel, newRaw)
			}
		}
		if len(oldVals) == 0 {
			if st != nil {
				st.UpdateNoOld++
				if fpw == nil || len(fpw.Get(flashbackFPWKey(rel.RelNode, blk.blkno))) == 0 {
					st.FPWMiss++
				}
			}
			return nil
		}
		oldOff, newOff := uint16(0), uint16(0)
		if len(main) >= flashbackSizeOfHeapUpdate {
			oldOff = binary.LittleEndian.Uint16(main[4:6])
			newOff = binary.LittleEndian.Uint16(main[12:14])
		}
		return emit(flashbackChange{
			XID: xid, Schema: rel.Schema, Table: rel.Name, Op: "UPDATE",
			Old: oldVals, New: newVals, PKCols: rel.PKCols, NoPK: len(rel.PKCols) == 0, RelNode: rel.RelNode,
			Block: blk.blkno, Offnum: oldOff, NewBlock: blk.blkno, NewOff: newOff,
		})
	default:
		return nil
	}
}

func flashbackApplyCatalogHeap(cat *flashbackCatalog, rel *flashbackRelation, xid uint32, rmid, op, info byte, blk *xlogBlockData, main []byte, fpw *flashbackFPWCache, st *flashbackParseStats) {
	if cat == nil || rel == nil {
		return
	}
	if rmid == rmHeap2 {
		if op == xlogHeap2MultiInsert {
			ch := flashbackDecodeMultiInsert(rel, xid, blk, main, info, func(c flashbackChange) []flashbackChange {
				return []flashbackChange{c}
			})
			for _, c := range ch {
				cat.apply(xid, rel, "INSERT", nil, c.New, rel.RelNode)
			}
		}
		return
	}
	switch op {
	case xlogHeapInsert:
		vals := flashbackDecodeXLogTuple(rel, blk.data)
		if len(vals) == 0 {
			return
		}
		cat.apply(xid, rel, "INSERT", nil, vals, rel.RelNode)
	case xlogHeapDelete:
		offnum := uint16(0)
		flags := byte(0)
		if len(main) >= flashbackSizeOfHeapDelete {
			offnum = binary.LittleEndian.Uint16(main[4:6])
			flags = main[7]
		}
		var vals map[string]string
		if flags&xlhDeleteContainsOldTuple != 0 && len(main) > flashbackSizeOfHeapDelete {
			vals = flashbackDecodeXLogTuple(rel, main[flashbackSizeOfHeapDelete:])
		}
		if len(vals) == 0 {
			vals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(flashbackRebuildFPW(blk), offnum))
		}
		if len(vals) == 0 && fpw != nil && offnum > 0 {
			vals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(fpw.Get(flashbackFPWKey(rel.RelNode, blk.blkno)), offnum))
		}
		if len(vals) == 0 && flags&xlhDeleteContainsOldKey != 0 && len(main) > flashbackSizeOfHeapDelete {
			vals = flashbackDecodeXLogTuple(flashbackRelationPKOnly(rel), main[flashbackSizeOfHeapDelete:])
		}
		if len(vals) == 0 {
			if st != nil {
				st.DeleteNoOld++
			}
			return
		}
		cat.apply(xid, rel, "DELETE", vals, nil, rel.RelNode)
	case xlogHeapUpdate, xlogHeapHotUpdate:
		oldVals := map[string]string{}
		newRaw := blk.data
		if len(main) >= flashbackSizeOfHeapUpdate {
			flags := main[7]
			newRaw = flashbackSkipUpdatePrefixSuffix(blk.data, flags)
		}
		newVals := flashbackDecodeXLogTuple(rel, newRaw)
		if len(main) >= flashbackSizeOfHeapUpdate {
			flags := main[7]
			oldOff := binary.LittleEndian.Uint16(main[4:6])
			if flags&xlhUpdateContainsOldTuple != 0 {
				oldVals = flashbackDecodeXLogTuple(rel, main[flashbackSizeOfHeapUpdate:])
			}
			if len(oldVals) == 0 {
				oldVals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(flashbackRebuildFPW(blk), oldOff))
			}
			if len(oldVals) == 0 && fpw != nil {
				oldVals = flashbackDecodeHeapTuple(rel, flashbackTupleFromPage(fpw.Get(flashbackFPWKey(rel.RelNode, blk.blkno)), oldOff))
			}
			if len(oldVals) == 0 && flags&xlhUpdateContainsOldKey != 0 {
				oldVals = flashbackDecodeXLogTuple(flashbackRelationPKOnly(rel), main[flashbackSizeOfHeapUpdate:])
			}
			if flags&xlhUpdateContainsNewTuple != 0 && len(newVals) == 0 {
				newVals = flashbackDecodeXLogTuple(rel, newRaw)
			}
		}
		if len(oldVals) == 0 {
			if st != nil {
				st.UpdateNoOld++
			}
			if len(newVals) == 0 {
				return
			}
		}
		cat.apply(xid, rel, "UPDATE", oldVals, newVals, rel.RelNode)
	}
}

func flashbackSkipUpdatePrefixSuffix(raw []byte, flags byte) []byte {
	// heapam_xlog.h：PREFIX/SUFFIX_FROM_OLD 时，新行数据前有 1～2 个 uint16。
	if flags&xlhUpdatePrefixFromOld != 0 {
		if len(raw) < 2 {
			return raw
		}
		raw = raw[2:]
	}
	if flags&xlhUpdateSuffixFromOld != 0 {
		if len(raw) < 2 {
			return raw
		}
		raw = raw[2:]
	}
	return raw
}

func flashbackDecodeMultiInsert(rel *flashbackRelation, xid uint32, blk *xlogBlockData, main []byte, info byte, emit func(flashbackChange) []flashbackChange) []flashbackChange {
	if rel == nil || len(main) < 3 {
		return nil
	}
	flags := main[0]
	ntuples := int(binary.LittleEndian.Uint16(main[1:3]))
	if ntuples <= 0 || ntuples > 10000 {
		return nil
	}
	// INIT_PAGE 时 main 里省略 offsets[ntuples]（官方 SizeOfHeapMultiInsert）。
	hdrOff := 3
	if info&xlogHeapInitPage == 0 {
		hdrOff = 3 + ntuples*2
	}
	src := main
	if flags&xlhInsertContainsNewTuple == 0 {
		if blk == nil {
			return nil
		}
		src = blk.data
		hdrOff = 0
	}
	if hdrOff > len(src) {
		return nil
	}
	data := src[hdrOff:]
	var out []flashbackChange
	for i := 0; i < ntuples && len(data) >= 8; i++ {
		datalen := int(binary.LittleEndian.Uint16(data[0:2]))
		infomask2 := binary.LittleEndian.Uint16(data[2:4])
		infomask := binary.LittleEndian.Uint16(data[4:6])
		hoff := int(data[6])
		const tupleHdr = 8
		if tupleHdr+datalen > len(data) {
			break
		}
		body := data[tupleHdr : tupleHdr+datalen]
		if hoff < flashbackSizeofHeapTuple {
			hoff = flashbackSizeofHeapTuple
		}
		full := make([]byte, hoff+len(body))
		copy(full[hoff:], body)
		binary.LittleEndian.PutUint16(full[18:20], infomask2)
		binary.LittleEndian.PutUint16(full[20:22], infomask)
		full[22] = byte(hoff)
		vals := flashbackDecodeHeapTuple(rel, full)
		if len(vals) > 0 {
			out = append(out, emit(flashbackChange{
				XID: xid, Schema: rel.Schema, Table: rel.Name, Op: "INSERT",
				New: vals, PKCols: rel.PKCols, NoPK: len(rel.PKCols) == 0, RelNode: rel.RelNode,
			})...)
		}
		next := flashbackMaxAlign(tupleHdr + datalen)
		if next > len(data) {
			break
		}
		data = data[next:]
	}
	return out
}

func flashbackDecodeHeapTruncate(xid, dboid uint32, dict *flashbackDictionary, main []byte, emit func(flashbackChange) []flashbackChange) []flashbackChange {
	// xl_heap_truncate：dbId(4)+nrelids(4)+flags(1)=9，XLogRegisterData 按 MAXALIGN 补到 16 再跟 Oid[]。
	const sizeOfHeapTruncate = 9
	if len(main) < sizeOfHeapTruncate || dict == nil || emit == nil {
		return nil
	}
	nrel := int(binary.LittleEndian.Uint32(main[4:8]))
	if nrel <= 0 || nrel > 4096 {
		return nil
	}
	tryOff := []int{flashbackMaxAlign(sizeOfHeapTruncate), 12, sizeOfHeapTruncate}
	var out []flashbackChange
	for _, start := range tryOff {
		got := flashbackHeapTruncateAt(xid, dboid, dict, main, nrel, start, emit)
		if len(got) > 0 {
			return got
		}
		if out == nil {
			out = got
		}
	}
	return out
}

func flashbackHeapTruncateAt(xid, dboid uint32, dict *flashbackDictionary, main []byte, nrel, off int, emit func(flashbackChange) []flashbackChange) []flashbackChange {
	var out []flashbackChange
	seen := map[uint32]struct{}{}
	for i := 0; i < nrel && off+4 <= len(main); i++ {
		relid := binary.LittleEndian.Uint32(main[off : off+4])
		off += 4
		if relid == 0 {
			continue
		}
		if _, ok := seen[relid]; ok {
			continue
		}
		seen[relid] = struct{}{}
		rel := dict.ByRelNode[relid]
		if rel == nil && dict.Catalog != nil {
			rel = dict.Catalog.userRelation(relid)
		}
		if rel == nil || rel.Missing {
			continue
		}
		if dboid != 0 && rel.DBOID != 0 && rel.DBOID != dboid {
			continue
		}
		qual := flashbackQualified(rel.Schema, rel.Name)
		out = append(out, emit(flashbackChange{
			XID: xid, Schema: rel.Schema, Table: rel.Name, Op: "TRUNCATE", RelNode: rel.RelNode,
			DDLRedo: fmt.Sprintf("TRUNCATE TABLE %s;", qual),
			DDLUndo: fmt.Sprintf("-- TRUNCATE %s 无法从 WAL 还原被清空的行，请用时间窗之前的备份/PITR", qual),
			DDLRisk: "TRUNCATE 只记录关系被清空，WAL 不含行图像，不能生成还原 INSERT",
		})...)
	}
	return out
}

func flashbackSplitXLogBody(body []byte) ([]xlogBlockData, []byte, uint32, bool) {
	type meta struct {
		id, flags, bimg      byte
		dataLen, imgLen      int
		holeOff, holeLen     uint16
		spc, db, rel, blkno  uint32
		compressed, hasImage bool
	}
	var metas []meta
	var last struct{ spc, db, rel uint32 }
	var topXID uint32
	i := 0
	mainLen := 0
	for i < len(body) {
		id := body[i]
		i++
		switch id {
		case xlrBlockIDDataShort:
			if i >= len(body) {
				return nil, nil, 0, false
			}
			mainLen = int(body[i])
			i++
			goto payload
		case xlrBlockIDDataLong:
			if i+4 > len(body) {
				return nil, nil, 0, false
			}
			mainLen = int(binary.LittleEndian.Uint32(body[i : i+4]))
			i += 4
			goto payload
		case xlrBlockIDOrigin:
			if i+2 > len(body) {
				return nil, nil, 0, false
			}
			i += 2
			continue
		case xlrBlockIDTopXID:
			if i+4 > len(body) {
				return nil, nil, 0, false
			}
			topXID = binary.LittleEndian.Uint32(body[i : i+4])
			i += 4
			continue
		}
		if id > xlrMaxBlockID {
			return nil, nil, 0, false
		}
		if i+3 > len(body) {
			return nil, nil, 0, false
		}
		m := meta{id: id, flags: body[i], dataLen: int(binary.LittleEndian.Uint16(body[i+1 : i+3]))}
		i += 3
		if m.flags&bkpHasImage != 0 {
			if i+5 > len(body) {
				return nil, nil, 0, false
			}
			m.hasImage = true
			m.imgLen = int(binary.LittleEndian.Uint16(body[i : i+2]))
			m.holeOff = binary.LittleEndian.Uint16(body[i+2 : i+4])
			bimg := body[i+4]
			i += 5
			m.bimg = bimg
			m.compressed = bimg&(bkpImagePGLZ|bkpImageLZ4|bkpImageZSTD) != 0
			if m.compressed && bimg&bkpImageHasHole != 0 {
				if i+2 > len(body) {
					return nil, nil, 0, false
				}
				m.holeLen = binary.LittleEndian.Uint16(body[i : i+2])
				i += 2
			} else if !m.compressed {
				if flashbackXLogPageSize >= m.imgLen {
					m.holeLen = uint16(flashbackXLogPageSize - m.imgLen)
				}
			}
		}
		if m.flags&bkpSameRel == 0 {
			if i+16 > len(body) {
				return nil, nil, 0, false
			}
			m.spc = binary.LittleEndian.Uint32(body[i : i+4])
			m.db = binary.LittleEndian.Uint32(body[i+4 : i+8])
			m.rel = binary.LittleEndian.Uint32(body[i+8 : i+12])
			m.blkno = binary.LittleEndian.Uint32(body[i+12 : i+16])
			i += 16
			last.spc, last.db, last.rel = m.spc, m.db, m.rel
		} else {
			if i+4 > len(body) {
				return nil, nil, 0, false
			}
			m.spc, m.db, m.rel = last.spc, last.db, last.rel
			m.blkno = binary.LittleEndian.Uint32(body[i : i+4])
			i += 4
		}
		metas = append(metas, m)
	}
payload:
	rest := body[i:]
	var blocks []xlogBlockData
	for _, m := range metas {
		b := xlogBlockData{
			spcNode: m.spc, dbNode: m.db, relNode: m.rel, blkno: m.blkno,
			holeOff: m.holeOff, holeLen: m.holeLen, bimgInfo: m.bimg, compressed: m.compressed,
		}
		if m.hasImage && m.imgLen > 0 {
			if m.imgLen > len(rest) {
				return nil, nil, 0, false
			}
			b.image = rest[:m.imgLen]
			rest = rest[m.imgLen:]
		}
		if m.flags&bkpHasData != 0 && m.dataLen > 0 {
			if m.dataLen > len(rest) {
				return nil, nil, 0, false
			}
			b.data = rest[:m.dataLen]
			rest = rest[m.dataLen:]
		}
		blocks = append(blocks, b)
	}
	var main []byte
	if mainLen > 0 {
		if mainLen > len(rest) {
			return nil, nil, 0, false
		}
		main = rest[:mainLen]
	}
	return blocks, main, topXID, true
}

func flashbackRebuildFPW(b *xlogBlockData) []byte {
	if b == nil || len(b.image) == 0 {
		return nil
	}
	raw := b.image
	if b.compressed {
		want := flashbackXLogPageSize - int(b.holeLen)
		if want <= 0 || want > flashbackXLogPageSize {
			return nil
		}
		dec := make([]byte, want)
		n, err := flashbackDecompressFPW(b.bimgInfo, b.image, dec)
		if err != nil || n != want {
			return nil
		}
		raw = dec
	}
	return flashbackInsertPageHole(raw, int(b.holeOff), int(b.holeLen))
}

func flashbackTupleFromPage(page []byte, offnum uint16) []byte {
	if len(page) < 24 || offnum == 0 {
		return nil
	}
	itemOff := 24 + int(offnum-1)*4
	if itemOff+4 > len(page) {
		return nil
	}
	item := binary.LittleEndian.Uint32(page[itemOff : itemOff+4])
	lpOff := int(item & 0x7FFF)
	lpFlags := int((item >> 15) & 0x3)
	lpLen := int((item >> 17) & 0x7FFF)
	if lpFlags != lpNormal || lpLen < flashbackSizeofHeapTuple {
		return nil
	}
	if lpOff+lpLen > len(page) {
		return nil
	}
	return page[lpOff : lpOff+lpLen]
}

func flashbackRelationPKOnly(rel *flashbackRelation) *flashbackRelation {
	if rel == nil {
		return nil
	}
	out := *rel
	var cols []flashbackColumn
	for _, c := range rel.Columns {
		if c.IsPK {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return rel
	}
	out.Columns = cols
	return &out
}

func flashbackDecodeXLogTuple(rel *flashbackRelation, raw []byte) map[string]string {
	if rel == nil || len(raw) < flashbackSizeOfHeapHeader {
		return nil
	}
	// WAL 存 xl_heap_header(5) + HeapTupleHeader 之后的字节，对齐按完整 HeapTuple 计算。
	rest := raw[flashbackSizeOfHeapHeader:]
	full := make([]byte, flashbackSizeofHeapTuple+len(rest))
	copy(full[flashbackSizeofHeapTuple:], rest)
	copy(full[18:20], raw[0:2])
	copy(full[20:22], raw[2:4])
	full[22] = raw[4]
	return flashbackDecodeHeapTuple(rel, full)
}

func flashbackDecodeHeapTuple(rel *flashbackRelation, raw []byte) map[string]string {
	if rel == nil || len(raw) < flashbackSizeofHeapTuple {
		return nil
	}
	infomask := binary.LittleEndian.Uint16(raw[20:22])
	hoff := int(raw[22])
	if hoff < flashbackSizeofHeapTuple || hoff > len(raw) {
		hoff = 24
		if hoff > len(raw) {
			return nil
		}
	}
	return flashbackDecodeAttrs(rel, raw, infomask, flashbackSizeofHeapTuple, hoff)
}
