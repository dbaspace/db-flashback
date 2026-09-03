package service

// 堆页扫描参考 PDU-PostgreSQLDataUnloader decode.c / read.c
// https://github.com/wublabdubdub/PDU-PostgreSQLDataUnloader
// Licensed under Apache License 2.0

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	flashbackHeapPageSize      = 8192
	flashbackPageHeaderSize    = 24
	flashbackItemIDSize        = 4
	flashbackLPNormal          = 1
	flashbackHeapXminCommitted = 0x0100
	flashbackHeapXmaxInvalid   = 0x0800
)

type flashbackHeapTuple struct {
	XMin     uint32
	XMax     uint32
	Block    uint32
	Offnum   uint16
	Infomask uint16
	Hoff     int
	Raw      []byte
	Dead     bool
	LSN      uint64
	Values   map[string]string
}

func flashbackRelationSegments(path string) []string {
	var out []string
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		out = append(out, path)
	}
	for i := 1; i < 1024; i++ {
		seg := path + "." + strconv.Itoa(i)
		if _, err := os.Stat(seg); err != nil {
			break
		}
		out = append(out, seg)
	}
	return out
}

func flashbackScanHeapFile(path string, rel *flashbackRelation, includeDead bool) ([]flashbackHeapTuple, error) {
	var out []flashbackHeapTuple
	for i, seg := range flashbackRelationSegments(path) {
		rows, err := flashbackScanHeapSegment(seg, uint32(i), rel, includeDead)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	if len(flashbackRelationSegments(path)) == 0 {
		return nil, fmt.Errorf("找不到关系文件 %s", path)
	}
	return out, nil
}

func flashbackScanHeapSegment(path string, segIndex uint32, rel *flashbackRelation, includeDead bool) ([]flashbackHeapTuple, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, flashbackHeapPageSize)
	var out []flashbackHeapTuple
	for blk := uint32(0); ; blk++ {
		n, err := io.ReadFull(f, buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if n == 0 {
				break
			}
			break
		}
		if err != nil {
			return out, err
		}
		pageBlk := segIndex*uint32((1<<30)/flashbackHeapPageSize) + blk
		out = append(out, flashbackScanHeapPage(buf, pageBlk, rel, includeDead)...)
	}
	return out, nil
}

func flashbackScanHeapPage(page []byte, blk uint32, rel *flashbackRelation, includeDead bool) []flashbackHeapTuple {
	if len(page) < flashbackPageHeaderSize {
		return nil
	}
	pdLower := binary.LittleEndian.Uint16(page[12:14])
	pdUpper := binary.LittleEndian.Uint16(page[14:16])
	pdSpecial := binary.LittleEndian.Uint16(page[16:18])
	if int(pdLower) < flashbackPageHeaderSize || int(pdUpper) > len(page) || pdSpecial > uint16(len(page)) || pdLower > pdUpper {
		return nil
	}
	lsn := binary.LittleEndian.Uint64(page[0:8])
	nitems := (int(pdLower) - flashbackPageHeaderSize) / flashbackItemIDSize
	var out []flashbackHeapTuple
	for i := 0; i < nitems; i++ {
		off := flashbackPageHeaderSize + i*flashbackItemIDSize
		item := binary.LittleEndian.Uint32(page[off : off+4])
		lpOff := uint16(item & 0x7FFF)
		lpFlags := (item >> 15) & 0x3
		lpLen := uint16((item >> 17) & 0x7FFF)
		if lpFlags != flashbackLPNormal || lpLen < flashbackSizeofHeapTuple {
			continue
		}
		if int(lpOff)+int(lpLen) > len(page) {
			continue
		}
		raw := make([]byte, lpLen)
		copy(raw, page[lpOff:int(lpOff)+int(lpLen)])
		if len(raw) < flashbackSizeofHeapTuple {
			continue
		}
		xmin := binary.LittleEndian.Uint32(raw[0:4])
		xmax := binary.LittleEndian.Uint32(raw[4:8])
		infomask := binary.LittleEndian.Uint16(raw[20:22])
		hoff := int(raw[22])
		if hoff < flashbackSizeofHeapTuple || hoff > len(raw) {
			continue
		}
		dead := xmax != 0 && infomask&flashbackHeapXmaxInvalid == 0
		if dead && !includeDead {
			continue
		}
		tup := flashbackHeapTuple{
			XMin: xmin, XMax: xmax, Block: blk, Offnum: uint16(i + 1),
			Infomask: infomask, Hoff: hoff, Raw: raw, Dead: dead, LSN: lsn,
		}
		if rel != nil {
			tup.Values = flashbackDecodeAttrs(rel, raw, infomask, flashbackSizeofHeapTuple, hoff)
		}
		out = append(out, tup)
	}
	return out
}

func flashbackValueIsNull(v string) bool {
	return v == `\N` || strings.EqualFold(v, "NULL") || strings.TrimSpace(v) == ""
}

func flashbackValuesAllNull(vals map[string]string) bool {
	if len(vals) == 0 {
		return true
	}
	for _, v := range vals {
		if !flashbackValueIsNull(v) {
			return false
		}
	}
	return true
}

func flashbackValuesIncomplete(rel *flashbackRelation, vals map[string]string) bool {
	if flashbackValuesAllNull(vals) {
		return true
	}
	if rel == nil {
		for _, v := range vals {
			if flashbackValueIsNull(v) {
				return true
			}
		}
		return false
	}
	for _, c := range rel.Columns {
		if c.Dropped {
			continue
		}
		v, ok := vals[c.Name]
		if !ok || flashbackValueIsNull(v) {
			return true
		}
	}
	return false
}

func flashbackMergeNonNull(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		if !flashbackValueIsNull(v) {
			dst[k] = v
		}
	}
	return dst
}

func flashbackHeapValsMatch(vals, known map[string]string) bool {
	if len(known) == 0 {
		return false
	}
	for k, want := range known {
		got, ok := vals[k]
		if !ok || flashbackValueIsNull(got) || got != want {
			return false
		}
	}
	return true
}

func flashbackFillOldFromDB(ctx context.Context, db *sql.DB, dict *flashbackDictionary, ch *flashbackChange) {
	if db == nil || dict == nil || ch == nil {
		return
	}
	rel := dict.match(ch.Schema, ch.Table)
	if rel == nil {
		return
	}
	if ch.Old == nil {
		ch.Old = map[string]string{}
	}
	page := flashbackReadHeapPageDB(ctx, db, rel, dict.DBOID, ch.Block)
	flashbackMergeOldFromPage(rel, ch, page, ch.Block)
}

func flashbackMergeOldFromPage(rel *flashbackRelation, ch *flashbackChange, page []byte, blk uint32) {
	if ch == nil || len(page) == 0 {
		return
	}
	for _, t := range flashbackScanHeapPage(page, blk, rel, true) {
		if ch.Offnum != 0 && t.Offnum != ch.Offnum {
			continue
		}
		ch.Old = flashbackMergeNonNull(ch.Old, t.Values)
		if ch.Offnum != 0 {
			return
		}
	}
}

func flashbackHeapRelFile(dboid, relnode, blk uint32) (relPath string, off int64) {
	pagesPerSeg := uint32((1024 * 1024 * 1024) / flashbackHeapPageSize)
	seg := blk / pagesPerSeg
	name := strconv.FormatUint(uint64(relnode), 10)
	if dboid == 0 {
		relPath = "global/" + name
	} else {
		relPath = "base/" + strconv.FormatUint(uint64(dboid), 10) + "/" + name
	}
	if seg > 0 {
		relPath += "." + strconv.FormatUint(uint64(seg), 10)
	}
	return relPath, int64(blk%pagesPerSeg) * int64(flashbackHeapPageSize)
}

func flashbackReadHeapPageDB(ctx context.Context, db *sql.DB, rel *flashbackRelation, dboid, blk uint32) []byte {
	if db == nil || rel == nil {
		return nil
	}
	pagesPerSeg := uint32((1024 * 1024 * 1024) / flashbackHeapPageSize)
	seg := int(blk / pagesPerSeg)
	off := int64(blk%pagesPerSeg) * int64(flashbackHeapPageSize)
	var raw []byte
	if rel.OID != 0 {
		q := `SELECT pg_read_binary_file(
CASE WHEN $2 = 0 THEN pg_relation_filepath($1::oid)
     ELSE pg_relation_filepath($1::oid) || '.' || $2::text END, $3, $4)`
		if err := db.QueryRowContext(ctx, q, rel.OID, seg, off, flashbackHeapPageSize).Scan(&raw); err == nil && len(raw) == flashbackHeapPageSize {
			return raw
		}
	}
	if rel.RelNode != 0 {
		path, fileOff := flashbackHeapRelFile(dboid, rel.RelNode, blk)
		if err := db.QueryRowContext(ctx, `SELECT pg_read_binary_file($1, $2, $3)`, path, fileOff, flashbackHeapPageSize).Scan(&raw); err == nil && len(raw) == flashbackHeapPageSize {
			return raw
		}
	}
	return nil
}

func flashbackPDUFillOldFromHeap(pgdata string, dict *flashbackDictionary, ch *flashbackChange) {
	if ch == nil || dict == nil || strings.TrimSpace(pgdata) == "" {
		return
	}
	rel := dict.match(ch.Schema, ch.Table)
	if rel == nil || rel.RelNode == 0 {
		return
	}
	path := flashbackHeapRelationPath(pgdata, dict.DBOID, rel.RelNode)
	if ch.Old == nil {
		ch.Old = map[string]string{}
	}
	if ch.Offnum > 0 {
		if tup := flashbackReadHeapTupleAt(path, ch.Block, ch.Offnum, rel); tup != nil {
			ch.Old = flashbackMergeNonNull(ch.Old, tup.Values)
		}
	}
	if !flashbackValuesIncomplete(rel, ch.Old) {
		return
	}
	if vals := flashbackPDUFindHeapByKnown(path, rel, ch.Old); len(vals) > 0 {
		ch.Old = flashbackMergeNonNull(ch.Old, vals)
	}
}

func flashbackPDUFindHeapByKnown(path string, rel *flashbackRelation, known map[string]string) map[string]string {
	keys := map[string]string{}
	for k, v := range known {
		if !flashbackValueIsNull(v) {
			keys[k] = v
		}
	}
	if len(keys) == 0 {
		return nil
	}
	tups, err := flashbackScanHeapFile(path, rel, true)
	if err != nil {
		return nil
	}
	var live, dead map[string]string
	for _, t := range tups {
		if !flashbackHeapValsMatch(t.Values, keys) {
			continue
		}
		if flashbackValuesIncomplete(rel, t.Values) {
			continue
		}
		if t.Dead {
			dead = t.Values
		} else {
			live = t.Values
		}
	}
	if dead != nil {
		return dead
	}
	return live
}

func flashbackReadHeapTupleAt(path string, blk uint32, offnum uint16, rel *flashbackRelation) *flashbackHeapTuple {
	if strings.TrimSpace(path) == "" || offnum == 0 || rel == nil {
		return nil
	}
	pagesPerSeg := uint32((1024 * 1024 * 1024) / flashbackHeapPageSize)
	seg := blk / pagesPerSeg
	pageInSeg := blk % pagesPerSeg
	segPath := path
	if seg > 0 {
		segPath = path + "." + strconv.Itoa(int(seg))
	}
	f, err := os.Open(segPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(int64(pageInSeg)*int64(flashbackHeapPageSize), io.SeekStart); err != nil {
		return nil
	}
	page := make([]byte, flashbackHeapPageSize)
	if _, err := io.ReadFull(f, page); err != nil {
		return nil
	}
	for _, t := range flashbackScanHeapPage(page, blk, rel, true) {
		if t.Offnum == offnum {
			cp := t
			return &cp
		}
	}
	return nil
}

func flashbackHeapRelationPath(pgdata string, dboid, relnode uint32) string {
	if relnode == 0 {
		return ""
	}
	if dboid == 0 {
		return filepath.Join(pgdata, "global", strconv.FormatUint(uint64(relnode), 10))
	}
	return filepath.Join(pgdata, "base", strconv.FormatUint(uint64(dboid), 10), strconv.FormatUint(uint64(relnode), 10))
}

func flashbackReadPGVersion(pgdata string) (int, string, error) {
	raw, err := os.ReadFile(filepath.Join(pgdata, "PG_VERSION"))
	if err != nil {
		return 0, "", err
	}
	s := strings.TrimSpace(string(raw))
	major, _ := strconv.Atoi(strings.Split(s, ".")[0])
	if major <= 0 {
		return 0, s, fmt.Errorf("无法解析 PG_VERSION=%s", s)
	}
	return major, s, nil
}

func flashbackCol(name, typ string, typlen int, align string) flashbackColumn {
	return flashbackColumn{
		Name: name, TypeName: typ, Typlen: typlen, Typalign: align, TypType: "b", BaseName: typ,
	}
}

func flashbackBuildRel(cols []flashbackColumn) *flashbackRelation {
	rel := &flashbackRelation{colByNum: map[int]flashbackColumn{}}
	for i := range cols {
		cols[i].AttNum = i + 1
		rel.Columns = append(rel.Columns, cols[i])
		rel.colByNum[cols[i].AttNum] = cols[i]
	}
	return rel
}

func flashbackParseU32Map(vals map[string]string, key string) uint32 {
	if vals == nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(vals[key]), 10, 32)
	return uint32(n)
}

func flashbackParseI32Map(vals map[string]string, key string) int {
	if vals == nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(vals[key]))
	return n
}
