package service

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// flashbackPGLZDecompress 对齐 PostgreSQL pglz_decompress（WAL FPW 不含 PGLZ_Header）。
func flashbackPGLZDecompress(src, dest []byte) (int, error) {
	if len(dest) == 0 {
		return 0, fmt.Errorf("pglz dest empty")
	}
	sp, dp := 0, 0
	for sp < len(src) && dp < len(dest) {
		ctrl := src[sp]
		sp++
		for bit := 0; bit < 8 && sp < len(src) && dp < len(dest); bit++ {
			if ctrl&1 != 0 {
				if sp+2 > len(src) {
					return 0, fmt.Errorf("pglz truncated match")
				}
				nlen := int(src[sp]&0x0f) + 3
				off := int((uint16(src[sp]&0xf0) << 4) | uint16(src[sp+1]))
				sp += 2
				if nlen == 18 {
					if sp >= len(src) {
						return 0, fmt.Errorf("pglz truncated extra len")
					}
					nlen += int(src[sp])
					sp++
				}
				if off <= 0 || off > dp {
					return 0, fmt.Errorf("pglz bad offset %d", off)
				}
				for nlen > 0 && dp < len(dest) {
					dest[dp] = dest[dp-off]
					dp++
					nlen--
				}
			} else {
				dest[dp] = src[sp]
				dp++
				sp++
			}
			ctrl >>= 1
		}
	}
	if dp != len(dest) || sp != len(src) {
		return 0, fmt.Errorf("pglz incomplete dp=%d want=%d sp=%d slen=%d", dp, len(dest), sp, len(src))
	}
	return dp, nil
}

func flashbackDecompressFPW(bimg byte, src, dest []byte) (int, error) {
	if len(src) == 0 || len(dest) == 0 {
		return 0, fmt.Errorf("empty fpw image")
	}
	switch {
	case bimg&bkpImagePGLZ != 0:
		return flashbackPGLZDecompress(src, dest)
	case bimg&bkpImageLZ4 != 0:
		n, err := lz4.UncompressBlock(src, dest)
		if err != nil {
			return 0, err
		}
		if n != len(dest) {
			return 0, fmt.Errorf("lz4 fpw size %d want %d", n, len(dest))
		}
		return n, nil
	case bimg&bkpImageZSTD != 0:
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return 0, err
		}
		defer dec.Close()
		out, err := dec.DecodeAll(src, dest[:0])
		if err != nil {
			return 0, err
		}
		if len(out) != len(dest) {
			return 0, fmt.Errorf("zstd fpw size %d want %d", len(out), len(dest))
		}
		copy(dest, out)
		return len(out), nil
	default:
		return 0, fmt.Errorf("unknown fpw compression 0x%02x", bimg)
	}
}

func flashbackInsertPageHole(packed []byte, holeOff, holeLen int) []byte {
	if holeLen <= 0 {
		if len(packed) >= flashbackXLogPageSize {
			return packed[:flashbackXLogPageSize]
		}
		page := make([]byte, flashbackXLogPageSize)
		copy(page, packed)
		return page
	}
	if holeOff < 0 || holeLen < 0 || holeOff+holeLen > flashbackXLogPageSize {
		return nil
	}
	if holeOff > len(packed) {
		return nil
	}
	page := make([]byte, flashbackXLogPageSize)
	copy(page[:holeOff], packed[:holeOff])
	copy(page[holeOff+holeLen:], packed[holeOff:])
	return page
}
