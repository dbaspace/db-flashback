package service

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	pgEpochUnixMicro = int64(946684800000000) // 2000-01-01 00:00:00 UTC in unix microseconds
	nbase            = 10000

	numericPOS         = 0x0000
	numericNEG         = 0x4000
	numericNAN         = 0xC000
	numericPINF        = 0xD000
	numericNINF        = 0xF000
	numericSHORT       = 0x8000
	numericDscaleMask  = 0x3FFF
	numericSignMask    = 0xC000
	varattIs1BE        = 0x01
	toastPointerMinLen = 18
)

var pgEpochUTC = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func flashbackTypeSupported(col flashbackColumn) (status, hint string) {
	name := flashbackBaseTypeName(col)
	if flashbackIsKnownType(name) {
		return "supported", name
	}
	if col.Typelem != 0 || strings.HasPrefix(name, "_") {
		return "supported", "array"
	}
	if col.TypType == "e" {
		return "supported", "enum"
	}
	if col.TypType == "d" && flashbackIsKnownType(col.BaseName) {
		return "supported", "domain:" + col.BaseName
	}
	return "unsupported", name
}

func flashbackBaseTypeName(col flashbackColumn) string {
	if col.TypType == "d" && col.BaseName != "" {
		return col.BaseName
	}
	name := strings.ToLower(strings.TrimSpace(col.TypeName))
	if name == "" {
		name = flashbackTypeNameByOID(col.TypeOID)
	}
	return name
}

func flashbackIsKnownType(name string) bool {
	switch strings.ToLower(strings.TrimPrefix(name, "_")) {
	case "bool", "boolean", "int2", "int4", "int8", "oid", "xid", "xid8", "cid", "tid",
		"float4", "float8", "money", "numeric",
		"text", "varchar", "bpchar", "char", "name", "bytea", "unknown",
		"date", "time", "timetz", "timestamp", "timestamptz", "interval",
		"json", "jsonb", "jsonpath", "xml", "uuid",
		"inet", "cidr", "macaddr", "macaddr8",
		"tsvector", "tsquery", "vector",
		"geometry", "geography", "raster",
		"int4range", "int8range", "numrange", "tsrange", "tstzrange", "daterange",
		"int4multirange", "int8multirange", "nummultirange", "tsmultirange", "tstzmultirange", "datemultirange",
		"pg_lsn", "bit", "varbit":
		return true
	default:
		return false
	}
}

func flashbackDecodeAttrs(rel *flashbackRelation, raw []byte, infomask uint16, bitsOff, dataOff int) map[string]string {
	hasNull := infomask&heapHasNull != 0
	natts := len(rel.Columns)
	bits := []byte(nil)
	if hasNull {
		n := (natts + 7) / 8
		if bitsOff+n <= len(raw) {
			bits = raw[bitsOff : bitsOff+n]
		}
	}
	off := dataOff
	out := map[string]string{}
	for i, col := range rel.Columns {
		if hasNull && len(bits) > 0 {
			if bits[i/8]&(1<<(i%8)) == 0 {
				if !col.Dropped {
					out[col.Name] = `\N`
				}
				continue
			}
		}
		off = flashbackAlignPointer(raw, off, col)
		if off >= len(raw) {
			break
		}
		val, n, ok := flashbackReadDatumToast(raw[off:], col, rel.toast)
		if !ok {
			if !col.Dropped {
				out[col.Name] = `\N`
			}
			break
		}
		off += n
		if col.Dropped {
			// 只占位对齐，不进入可执行 undo（列已不存在）；值按 hex 留给诊断。
			continue
		}
		out[col.Name] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func flashbackAlign(off int, typalign string) int {
	var a int
	switch typalign {
	case "c":
		return off
	case "s":
		a = 2
	case "i":
		a = 4
	default:
		a = 8
	}
	if m := off % a; m != 0 {
		off += a - m
	}
	return off
}

// flashbackAlignPointer 对齐 PG att_align_pointer：短 varlena 可从任意偏移开始，
// 当前字节为 0 才按 typalign 跳过填充。
func flashbackAlignPointer(raw []byte, off int, col flashbackColumn) int {
	if col.Typlen < 0 {
		if off < len(raw) && raw[off] != 0 {
			return off
		}
	}
	return flashbackAlign(off, col.Typalign)
}

func flashbackHexEncodeSQL(b []byte) string {
	return `\RAW:encode('\x` + hex.EncodeToString(b) + `'::bytea, 'hex')`
}

func flashbackReadDatum(b []byte, col flashbackColumn) (string, int, bool) {
	return flashbackReadDatumToast(b, col, nil)
}

func flashbackReadDatumToast(b []byte, col flashbackColumn, toast *flashbackToastCache) (string, int, bool) {
	if col.Dropped {
		if col.Typlen > 0 {
			if len(b) < col.Typlen {
				return "", 0, false
			}
			return flashbackHexEncodeSQL(b[:col.Typlen]), col.Typlen, true
		}
		raw, n, ok := flashbackReadVarlena(b)
		if !ok {
			return "", 0, false
		}
		if raw == nil {
			return `\N`, n, true
		}
		return flashbackHexEncodeSQL(raw), n, true
	}
	if col.Typlen > 0 {
		if len(b) < col.Typlen {
			return "", 0, false
		}
		s, ok := flashbackDecodeFixed(b[:col.Typlen], col)
		return s, col.Typlen, ok
	}
	if p, n, ok := flashbackParseToastPtr(b); ok {
		s, ok := flashbackResolveToast(p, toast, col)
		return s, n, ok
	}
	raw, n, ok := flashbackReadVarlena(b)
	if !ok {
		return "", 0, false
	}
	if raw == nil {
		return `\N`, n, true
	}
	s, ok := flashbackDecodeVarlena(raw, col)
	return s, n, ok
}

func flashbackReadVarlena(b []byte) (data []byte, consumed int, ok bool) {
	if len(b) < 1 {
		return nil, 0, false
	}
	h := b[0]
	if h == varattIs1BE {
		// 外置 TOAST 指针：1 字节 tag + 至少 17 字节 va_external。
		if len(b) < toastPointerMinLen {
			return nil, 0, false
		}
		return nil, toastPointerMinLen, true
	}
	if h&0x01 == 1 {
		n := int(h >> 1)
		if n < 1 || len(b) < n {
			return nil, 0, false
		}
		return b[1:n], n, true
	}
	if len(b) < 4 {
		return nil, 0, false
	}
	n := int(binary.LittleEndian.Uint32(b[:4]) >> 2)
	if n < 4 || len(b) < n {
		return nil, 0, false
	}
	return b[4:n], n, true
}

func flashbackDecodeFixed(b []byte, col flashbackColumn) (string, bool) {
	name := flashbackBaseTypeName(col)
	switch name {
	case "bool", "boolean":
		if b[0] != 0 {
			return "t", true
		}
		return "f", true
	case "char":
		if len(b) == 0 {
			return "", true
		}
		if b[0] == 0 {
			return "", true
		}
		return string([]byte{b[0]}), true
	case "name":
		n := 0
		for n < len(b) && b[n] != 0 {
			n++
		}
		return string(b[:n]), true
	case "int2":
		return strconv.Itoa(int(int16(binary.LittleEndian.Uint16(b)))), true
	case "int4", "oid", "xid", "cid", "regclass", "regtype", "regproc":
		if name == "int4" {
			return strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(b))), 10), true
		}
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(b)), 10), true
	case "int8", "xid8":
		if name == "xid8" {
			return strconv.FormatUint(binary.LittleEndian.Uint64(b), 10), true
		}
		return strconv.FormatInt(int64(binary.LittleEndian.Uint64(b)), 10), true
	case "pg_lsn":
		if len(b) < 8 {
			return "", false
		}
		v := binary.LittleEndian.Uint64(b[:8])
		return fmt.Sprintf("%X/%X", uint32(v>>32), uint32(v)), true
	case "float4":
		return strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), 'g', -1, 32), true
	case "float8":
		return strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(b)), 'g', -1, 64), true
	case "money":
		v := int64(binary.LittleEndian.Uint64(b))
		neg := ""
		if v < 0 {
			neg = "-"
			v = -v
		}
		return fmt.Sprintf("%s%d.%02d", neg, v/100, v%100), true
	case "date":
		days := int32(binary.LittleEndian.Uint32(b))
		t := pgEpochUTC.AddDate(0, 0, int(days))
		return t.Format("2006-01-02"), true
	case "time":
		us := int64(binary.LittleEndian.Uint64(b))
		return flashbackFormatTimeOfDay(us), true
	case "timetz":
		if len(b) < 12 {
			return "", false
		}
		us := int64(binary.LittleEndian.Uint64(b[:8]))
		tz := int32(binary.LittleEndian.Uint32(b[8:12]))
		return flashbackFormatTimeOfDay(us) + flashbackFormatTZOffset(tz), true
	case "timestamp", "timestamptz":
		us := int64(binary.LittleEndian.Uint64(b))
		t := time.UnixMicro(pgEpochUnixMicro + us).UTC()
		if name == "timestamptz" {
			return t.Format("2006-01-02 15:04:05.999999Z07:00"), true
		}
		return t.Format("2006-01-02 15:04:05.999999"), true
	case "interval":
		if len(b) < 16 {
			return "", false
		}
		us := int64(binary.LittleEndian.Uint64(b[0:8]))
		day := int32(binary.LittleEndian.Uint32(b[8:12]))
		mon := int32(binary.LittleEndian.Uint32(b[12:16]))
		return flashbackFormatInterval(us, day, mon), true
	case "uuid":
		if len(b) < 16 {
			return "", false
		}
		h := hex.EncodeToString(b[:16])
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], true
	case "tid":
		if len(b) < 6 {
			return "", false
		}
		blk := binary.LittleEndian.Uint32(b[0:4])
		off := binary.LittleEndian.Uint16(b[4:6])
		return fmt.Sprintf("(%d,%d)", blk, off), true
	case "macaddr":
		if len(b) < 6 {
			return "", false
		}
		return net.HardwareAddr(b[:6]).String(), true
	case "macaddr8":
		if len(b) < 8 {
			return "", false
		}
		return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7]), true
	}
	if col.TypType == "e" && col.EnumLabels != nil {
		oid := binary.LittleEndian.Uint32(b)
		if lab, ok := col.EnumLabels[oid]; ok {
			return lab, true
		}
		return strconv.FormatUint(uint64(oid), 10), true
	}
	return "\\x" + hex.EncodeToString(b), true
}

func flashbackDecodeVarlena(raw []byte, col flashbackColumn) (string, bool) {
	name := flashbackBaseTypeName(col)
	if col.Typelem != 0 || strings.HasPrefix(col.TypeName, "_") {
		return flashbackDecodeArray(raw, col)
	}
	switch name {
	case "numeric":
		return flashbackDecodeNumeric(raw)
	case "jsonb":
		s, ok := flashbackDecodeJSONB(raw)
		if ok {
			return s, true
		}
		return flashbackPrintBytes(raw), true
	case "json", "xml", "text", "varchar", "bpchar", "char", "name", "unknown", "cstring":
		return string(raw), true
	case "jsonpath", "tsvector", "tsquery":
		return flashbackPrintBytes(raw), true
	case "bytea", "geometry", "geography", "raster":
		return "\\x" + hex.EncodeToString(raw), true
	case "inet", "cidr":
		return flashbackDecodeInet(raw, name == "cidr")
	case "vector":
		return flashbackDecodeVector(raw)
	case "bit", "varbit":
		return flashbackDecodeBit(raw)
	case "pg_lsn":
		if len(raw) >= 8 {
			v := binary.LittleEndian.Uint64(raw[:8])
			return fmt.Sprintf("%X/%X", uint32(v>>32), uint32(v)), true
		}
	}
	if strings.Contains(name, "range") {
		return flashbackHexEncodeSQL(raw), true
	}
	return flashbackHexEncodeSQL(raw), true
}

func flashbackDecodeNumeric(b []byte) (string, bool) {
	if len(b) < 2 {
		return "", false
	}
	h := binary.LittleEndian.Uint16(b[0:2])
	signBits := h & 0xF000
	if signBits == numericNAN {
		return "NaN", true
	}
	if signBits == numericPINF {
		return "Infinity", true
	}
	if signBits == numericNINF {
		return "-Infinity", true
	}
	var weight int16
	var dscale int
	var neg bool
	var digits []int16
	if h&numericSHORT != 0 {
		neg = h&0x4000 != 0
		dscale = int((h & 0x3F80) >> 7)
		weight = int16(h & 0x003F)
		if h&0x0040 != 0 {
			weight = -weight
		}
		rest := b[2:]
		for i := 0; i+2 <= len(rest); i += 2 {
			digits = append(digits, int16(binary.LittleEndian.Uint16(rest[i:i+2])))
		}
	} else {
		if len(b) < 4 {
			return "", false
		}
		signDscale := h
		neg = signDscale&numericNEG != 0
		dscale = int(signDscale & numericDscaleMask)
		weight = int16(binary.LittleEndian.Uint16(b[2:4]))
		rest := b[4:]
		for i := 0; i+2 <= len(rest); i += 2 {
			digits = append(digits, int16(binary.LittleEndian.Uint16(rest[i:i+2])))
		}
	}
	s := flashbackNumericString(neg, weight, dscale, digits)
	if s == "" {
		return "", false
	}
	return s, true
}

func flashbackNumericString(neg bool, weight int16, dscale int, digits []int16) string {
	if len(digits) > 16 || dscale > 16 || int(weight) > 8 || int(weight) < -8 {
		return ""
	}
	if len(digits) == 0 {
		if dscale > 0 {
			return "0." + strings.Repeat("0", dscale)
		}
		return "0"
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	// 整数部分：digits[0] 的权重为 weight。
	intDigits := int(weight) + 1
	if intDigits <= 0 {
		b.WriteByte('0')
	} else {
		for i := 0; i < intDigits; i++ {
			d := 0
			if i < len(digits) {
				d = int(digits[i])
			}
			if i == 0 {
				b.WriteString(strconv.Itoa(d))
			} else {
				b.WriteString(fmt.Sprintf("%04d", d))
			}
		}
	}
	if dscale > 0 {
		b.WriteByte('.')
		var frac strings.Builder
		for i := intDigits; frac.Len() < dscale+4; i++ {
			d := 0
			if i >= 0 && i < len(digits) {
				d = int(digits[i])
			}
			frac.WriteString(fmt.Sprintf("%04d", d))
		}
		s := frac.String()
		if len(s) < dscale {
			s += strings.Repeat("0", dscale-len(s))
		}
		b.WriteString(s[:dscale])
	}
	return b.String()
}

func flashbackDecodeInet(b []byte, cidr bool) (string, bool) {
	if s, ok := flashbackTryDecodeInet(b, cidr); ok {
		return s, true
	}
	return flashbackPrintBytes(b), true
}

func flashbackInetFamilyIPv4(family byte) bool { return family == 2 }
func flashbackInetFamilyIPv6(family byte) bool {
	// PGSQL_AF_INET6 = AF_INET6：Linux=10，macOS=30；部分代码路径用 3。
	return family == 3 || family == 10 || family == 30
}

func flashbackFormatInet(ip net.IP, bits int, addrLen int, cidr bool) string {
	if cidr {
		return fmt.Sprintf("%s/%d", ip.String(), bits)
	}
	if bits == addrLen*8 {
		return ip.String()
	}
	return fmt.Sprintf("%s/%d", ip.String(), bits)
}

func flashbackTryDecodeInet(b []byte, cidr bool) (string, bool) {
	if len(b) < 2 {
		return "", false
	}
	family, bits := b[0], int(b[1])
	// 堆内格式 inet_struct：family + bits + ipaddr[16]
	if len(b) >= 18 && flashbackInetFamilyIPv4(family) {
		ip := net.IPv4(b[2], b[3], b[4], b[5])
		return flashbackFormatInet(ip, bits, 4, cidr), true
	}
	if len(b) >= 18 && flashbackInetFamilyIPv6(family) {
		ip := net.IP(append([]byte(nil), b[2:18]...))
		return flashbackFormatInet(ip, bits, 16, cidr), true
	}
	if len(b) == 6 && flashbackInetFamilyIPv4(family) {
		ip := net.IPv4(b[2], b[3], b[4], b[5])
		return flashbackFormatInet(ip, bits, 4, cidr), true
	}
	// 发送格式：family + bits + flag + nb + addr
	if len(b) >= 4 {
		nb := int(b[3])
		if nb > 0 && len(b) >= 4+nb {
			addr := b[4 : 4+nb]
			if flashbackInetFamilyIPv4(family) && nb >= 4 {
				ip := net.IPv4(addr[0], addr[1], addr[2], addr[3])
				return flashbackFormatInet(ip, bits, 4, cidr), true
			}
			if flashbackInetFamilyIPv6(family) && nb >= 16 {
				ip := net.IP(append([]byte(nil), addr[:16]...))
				return flashbackFormatInet(ip, bits, 16, cidr), true
			}
		}
	}
	return "", false
}

func flashbackDecodeArray(b []byte, col flashbackColumn) (string, bool) {
	if len(b) < 12 {
		return flashbackPrintBytes(b), true
	}
	ndim := int(int32(binary.LittleEndian.Uint32(b[0:4])))
	dataOff := int(int32(binary.LittleEndian.Uint32(b[4:8])))
	if ndim <= 0 || ndim > 6 {
		return flashbackPrintBytes(b), true
	}
	off := 12
	dims := make([]int, ndim)
	nitems := 1
	for i := 0; i < ndim; i++ {
		if off+8 > len(b) {
			return flashbackPrintBytes(b), true
		}
		dims[i] = int(int32(binary.LittleEndian.Uint32(b[off : off+4])))
		off += 8 // dim + lbound
		if dims[i] < 0 {
			return flashbackPrintBytes(b), true
		}
		nitems *= dims[i]
	}
	var nulls []bool
	if dataOff != 0 {
		nb := (nitems + 7) / 8
		if off+nb > len(b) {
			return flashbackPrintBytes(b), true
		}
		nulls = make([]bool, nitems)
		for i := 0; i < nitems; i++ {
			if b[off+i/8]&(1<<(7-(i%8))) == 0 {
				nulls[i] = true
			}
		}
		off += nb
		off = flashbackAlign(off, "i")
	}
	elem := col
	elem.TypeOID = col.Typelem
	elem.TypeName = strings.TrimPrefix(col.TypeName, "_")
	if col.ElemTyplen != 0 {
		elem.Typlen = col.ElemTyplen
		elem.Typalign = col.ElemTypalign
		elem.TypeName = col.ElemName
	} else {
		elem.Typlen = flashbackBuiltinTyplen(elem.TypeName)
		elem.Typalign = flashbackBuiltinTypalign(elem.TypeName)
	}
	elem.Typelem = 0
	vals := make([]string, 0, nitems)
	for i := 0; i < nitems; i++ {
		if nulls != nil && i < len(nulls) && nulls[i] {
			vals = append(vals, "NULL")
			continue
		}
		off = flashbackAlign(off, elem.Typalign)
		if off >= len(b) {
			break
		}
		s, n, ok := flashbackReadDatum(b[off:], elem)
		if !ok {
			break
		}
		vals = append(vals, s)
		off += n
	}
	return flashbackFormatArray(dims, vals), true
}

func flashbackFormatArray(dims []int, vals []string) string {
	if len(dims) == 0 {
		return "{}"
	}
	var walk func(d int, idx *int) string
	walk = func(d int, idx *int) string {
		if d == len(dims)-1 {
			n := dims[d]
			parts := make([]string, 0, n)
			for i := 0; i < n; i++ {
				if *idx < len(vals) {
					parts = append(parts, flashbackArrayQuote(vals[*idx]))
					*idx++
				}
			}
			return "{" + strings.Join(parts, ",") + "}"
		}
		n := dims[d]
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			parts = append(parts, walk(d+1, idx))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	idx := 0
	return walk(0, &idx)
}

func flashbackArrayQuote(v string) string {
	if v == `\N` || v == "NULL" {
		return "NULL"
	}
	if strings.ContainsAny(v, `{},"`) || strings.Contains(v, " ") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}

func flashbackDecodeJSONB(b []byte) (string, bool) {
	if s, ok := flashbackJSONBContainer(b); ok {
		return s, true
	}
	if len(b) >= 2 && b[0] == 1 {
		if s, ok := flashbackJSONBContainer(b[1:]); ok {
			return s, true
		}
	}
	return "", false
}

func flashbackJSONBContainer(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	h := binary.LittleEndian.Uint32(b[0:4])
	count := int(h & 0x0FFFFFFF)
	const (
		jbScalar = 0x10000000
		jbObject = 0x20000000
		jbArray  = 0x40000000
	)
	kind := h & 0xF0000000
	data := b[4:]
	switch {
	case kind&jbScalar != 0:
		if count != 1 || len(data) < 4 {
			return "", false
		}
		entry := binary.LittleEndian.Uint32(data[0:4])
		return flashbackJSONBValue(entry, data[4:], 0)
	case kind&jbArray != 0:
		if len(data) < 4*count {
			return "", false
		}
		var parts []string
		off := 0
		for i := 0; i < count; i++ {
			entry := binary.LittleEndian.Uint32(data[i*4 : i*4+4])
			s, ok := flashbackJSONBValue(entry, data[4*count:], off)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
			off = flashbackJEntryAdvance(entry, off)
		}
		return "[" + strings.Join(parts, ", ") + "]", true
	case kind&jbObject != 0:
		if len(data) < 8*count {
			return "", false
		}
		var parts []string
		off := 0
		valBase := 4 * count * 2
		for i := 0; i < count; i++ {
			kEnt := binary.LittleEndian.Uint32(data[i*4 : i*4+4])
			vEnt := binary.LittleEndian.Uint32(data[(count+i)*4 : (count+i)*4+4])
			ks, ok := flashbackJSONBValue(kEnt, data[valBase:], off)
			if !ok {
				return "", false
			}
			off = flashbackJEntryAdvance(kEnt, off)
			vs, ok := flashbackJSONBValue(vEnt, data[valBase:], off)
			if !ok {
				return "", false
			}
			off = flashbackJEntryAdvance(vEnt, off)
			parts = append(parts, ks+": "+vs)
		}
		return "{" + strings.Join(parts, ", ") + "}", true
	default:
		return "", false
	}
}

// flashbackDecodeJSONBNumeric 对齐 jsonb.c：numeric 前 INTALIGN，本体是 varlena Numeric。
func flashbackDecodeJSONBNumeric(data []byte, start, ln int) (string, bool) {
	if start < 0 || ln <= 0 || start+ln > len(data) {
		return "", false
	}
	end := start + ln
	try := func(off int) (string, bool) {
		if off < start || off >= end {
			return "", false
		}
		raw := data[off:end]
		if payload, _, ok := flashbackReadVarlena(raw); ok && payload != nil {
			if s, ok := flashbackDecodeNumeric(payload); ok && flashbackJSONBNumericOK(s) {
				return s, true
			}
		}
		if s, ok := flashbackDecodeNumeric(raw); ok && flashbackJSONBNumericOK(s) {
			return s, true
		}
		return "", false
	}
	if s, ok := try((start + 3) &^ 3); ok {
		return s, true
	}
	return try(start)
}

func flashbackJSONBNumericOK(s string) bool {
	return s != "" && len(s) <= 64
}

func flashbackJEntryAdvance(entry uint32, off int) int {
	const (
		jHasOff     = 0x80000000
		jOffLenMask = 0x0FFFFFFF
	)
	if entry&jHasOff != 0 {
		return int(entry & jOffLenMask)
	}
	return off + int(entry&jOffLenMask)
}

func flashbackJSONBValue(entry uint32, data []byte, off int) (string, bool) {
	const (
		jTypeMask    = 0x70000000
		jOffLenMask  = 0x0FFFFFFF
		jIsString    = 0x00000000
		jIsNumeric   = 0x10000000
		jIsBoolFalse = 0x20000000
		jIsBoolTrue  = 0x30000000
		jIsNull      = 0x40000000
		jIsContainer = 0x50000000
		jHasOff      = 0x80000000
	)
	typ := entry & jTypeMask
	offlen := int(entry & jOffLenMask)
	start := off
	ln := offlen
	if entry&jHasOff != 0 {
		ln = offlen - off
		if ln < 0 {
			return "", false
		}
	}
	switch typ {
	case jIsNull:
		return "null", true
	case jIsBoolTrue:
		return "true", true
	case jIsBoolFalse:
		return "false", true
	case jIsString:
		if start+ln > len(data) {
			return "", false
		}
		return strconv.Quote(string(data[start : start+ln])), true
	case jIsNumeric:
		if start+ln > len(data) {
			return "", false
		}
		s, ok := flashbackDecodeJSONBNumeric(data, start, ln)
		if !ok {
			return "", false
		}
		return s, true
	case jIsContainer:
		if start+ln > len(data) {
			return "", false
		}
		return flashbackJSONBContainer(data[start : start+ln])
	default:
		return "", false
	}
}

func flashbackDecodeVector(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	dim := int(binary.LittleEndian.Uint16(b[0:2]))
	off := 4
	if off+dim*4 > len(b) {
		return "", false
	}
	parts := make([]string, dim)
	for i := 0; i < dim; i++ {
		parts[i] = strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:off+4]))), 'g', -1, 32)
		off += 4
	}
	return "[" + strings.Join(parts, ",") + "]", true
}

func flashbackDecodeBit(b []byte) (string, bool) {
	if len(b) < 4 {
		return "", false
	}
	nbits := int(int32(binary.LittleEndian.Uint32(b[0:4])))
	if nbits < 0 {
		return "", false
	}
	need := (nbits + 7) / 8
	if 4+need > len(b) {
		return "", false
	}
	var s strings.Builder
	for i := 0; i < nbits; i++ {
		if b[4+i/8]&(1<<(7-(i%8))) != 0 {
			s.WriteByte('1')
		} else {
			s.WriteByte('0')
		}
	}
	return s.String(), true
}

func flashbackFormatTimeOfDay(us int64) string {
	if us < 0 {
		us = 0
	}
	h := us / 3600_000000
	us %= 3600_000000
	m := us / 60_000000
	us %= 60_000000
	s := us / 1_000000
	frac := us % 1_000000
	if frac == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d.%06d", h, m, s, frac)
}

func flashbackFormatTZOffset(sec int32) string {
	// PostgreSQL timetz 存的是西向秒数（与 UTC 的偏移）。
	off := -sec
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)
}

func flashbackFormatInterval(us int64, day, mon int32) string {
	var parts []string
	if mon != 0 {
		y := mon / 12
		m := mon % 12
		if y != 0 {
			parts = append(parts, fmt.Sprintf("%d years", y))
		}
		if m != 0 {
			parts = append(parts, fmt.Sprintf("%d mons", m))
		}
	}
	if day != 0 {
		parts = append(parts, fmt.Sprintf("%d days", day))
	}
	if us != 0 || len(parts) == 0 {
		neg := ""
		if us < 0 {
			neg = "-"
			us = -us
		}
		parts = append(parts, neg+flashbackFormatTimeOfDay(us))
	}
	return strings.Join(parts, " ")
}

func flashbackPrintBytes(b []byte) string {
	if utf8.Valid(b) && flashbackMostlyPrintable(b) {
		return string(b)
	}
	return "\\x" + hex.EncodeToString(b)
}

func flashbackMostlyPrintable(b []byte) bool {
	for _, r := range string(b) {
		if r == utf8.RuneError {
			return false
		}
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func flashbackBuiltinTyplen(name string) int {
	switch name {
	case "bool", "char":
		return 1
	case "int2":
		return 2
	case "int4", "oid", "xid", "cid", "float4", "date":
		return 4
	case "int8", "float8", "money", "time", "timestamp", "timestamptz", "xid8", "pg_lsn":
		return 8
	case "timetz":
		return 12
	case "interval", "uuid":
		return 16
	case "macaddr":
		return 6
	case "macaddr8", "tid":
		if name == "tid" {
			return 6
		}
		return 8
	default:
		return -1
	}
}

func flashbackBuiltinTypalign(name string) string {
	switch name {
	case "bool", "char", "uuid", "macaddr", "macaddr8":
		return "c"
	case "int2":
		return "s"
	case "int4", "oid", "xid", "cid", "float4", "date":
		return "i"
	default:
		return "d"
	}
}
