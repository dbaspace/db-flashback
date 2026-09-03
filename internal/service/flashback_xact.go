package service

import (
	"encoding/binary"
	"sort"
	"time"
)

// PostgreSQL TimestampTz：微秒，起点 2000-01-01 00:00:00 UTC。
var flashbackPGEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	xlogXactCommitPrepared = 0x30
	xlogXactAbortPrepared  = 0x40
	xlogXactHasInfo        = 0x80
	xlogXactOpMask         = 0x70

	xactXinfoHasDBInfo   = 1 << 0
	xactXinfoHasSubxacts = 1 << 1
)

type flashbackXactCommit struct {
	Time    time.Time
	SubXIDs []uint32
}

func flashbackPGTimestamp(us int64) time.Time {
	return flashbackPGEpoch.Add(time.Duration(us) * time.Microsecond)
}

func flashbackTimeToPGTimestamp(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Sub(flashbackPGEpoch).Microseconds()
}

// flashbackXactMainData 取出 COMMIT/ABORT 的 xl_xact_* 主数据。
// 真实 WAL 在 XLogRecord 之后是 block-id 包装；单测可直接跟裸 payload。
func flashbackXactMainData(rec []byte) []byte {
	if len(rec) < flashbackSizeOfXLogRecord {
		return nil
	}
	body := rec[flashbackSizeOfXLogRecord:]
	if len(body) == 0 {
		return nil
	}
	_, main, _, ok := flashbackSplitXLogBody(body)
	if ok && len(main) > 0 {
		return main
	}
	return body
}

func flashbackParseXactCommit(info byte, payload []byte, prepared bool) flashbackXactCommit {
	off := 0
	if prepared {
		off = 4 // xl_xact_commit_prepared.xid
	}
	if off+8 > len(payload) {
		return flashbackXactCommit{}
	}
	us := int64(binary.LittleEndian.Uint64(payload[off : off+8]))
	out := flashbackXactCommit{Time: flashbackPGTimestamp(us)}
	off += 8
	if info&xlogXactHasInfo == 0 {
		return out
	}
	if off+4 > len(payload) {
		return out
	}
	xinfo := binary.LittleEndian.Uint32(payload[off : off+4])
	off += 4
	if xinfo&xactXinfoHasDBInfo != 0 {
		off += 8
	}
	if xinfo&xactXinfoHasSubxacts == 0 || off+4 > len(payload) {
		return out
	}
	n := int(int32(binary.LittleEndian.Uint32(payload[off : off+4])))
	off += 4
	if n < 0 {
		n = 0
	}
	if n > 1<<20 {
		n = 1 << 20
	}
	for i := 0; i < n && off+4 <= len(payload); i++ {
		out.SubXIDs = append(out.SubXIDs, binary.LittleEndian.Uint32(payload[off:off+4]))
		off += 4
	}
	return out
}

type flashbackTxnBuf struct {
	pending map[uint32][]flashbackChange
}

func flashbackNewTxnBuf() *flashbackTxnBuf {
	return &flashbackTxnBuf{pending: map[uint32][]flashbackChange{}}
}

func (b *flashbackTxnBuf) add(ch flashbackChange) {
	if b == nil || ch.XID == 0 {
		return
	}
	if b.pending == nil {
		b.pending = map[uint32][]flashbackChange{}
	}
	b.pending[ch.XID] = append(b.pending[ch.XID], ch)
}

func (b *flashbackTxnBuf) discard(xids ...uint32) {
	if b == nil {
		return
	}
	for _, x := range xids {
		delete(b.pending, x)
	}
}

func (b *flashbackTxnBuf) flush(cmt flashbackXactCommit, xid uint32) []flashbackChange {
	if b == nil {
		return nil
	}
	ids := append([]uint32{xid}, cmt.SubXIDs...)
	var out []flashbackChange
	for _, id := range ids {
		for _, ch := range b.pending[id] {
			ch.TS = cmt.Time
			out = append(out, ch)
		}
		delete(b.pending, id)
	}
	return out
}

func (b *flashbackTxnBuf) dumpAll() []flashbackChange {
	if b == nil || len(b.pending) == 0 {
		return nil
	}
	var xids []uint32
	for x := range b.pending {
		xids = append(xids, x)
	}
	sort.Slice(xids, func(i, j int) bool { return xids[i] < xids[j] })
	var out []flashbackChange
	for _, x := range xids {
		out = append(out, b.pending[x]...)
		delete(b.pending, x)
	}
	return out
}

func flashbackFilterCommitTime(ch []flashbackChange, from, to time.Time) []flashbackChange {
	if from.IsZero() && to.IsZero() {
		return ch
	}
	out := ch[:0]
	for _, c := range ch {
		if c.TS.IsZero() {
			continue
		}
		if !from.IsZero() && c.TS.Before(from) {
			continue
		}
		if !to.IsZero() && c.TS.After(to) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (o flashbackParseOpts) hasTimeWindow() bool {
	return !o.TimeFrom.IsZero() || !o.TimeTo.IsZero()
}
