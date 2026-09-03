package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

func flashbackUnpackCloudWAL(src, destDir string) ([]flashbackWALFile, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var r io.Reader = f
	switch {
	case flashbackLooksLikeGzip(head):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	case flashbackLooksLikeZstd(head):
		zr, err := zstd.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("zstd: %w", err)
		}
		defer zr.Close()
		r = zr
	}
	// 再探一层：zstd/gzip 解开后可能是 tar，也可能是单段 WAL。
	buf := make([]byte, 512)
	n, err = io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	inner := io.MultiReader(bytes.NewReader(buf[:n]), r)
	if flashbackLooksLikeTar(buf[:n]) {
		return flashbackExtractWALFromTar(inner, destDir)
	}
	if name := filepath.Base(src); flashbackIsWALSegName(flashbackStripArchiveSuffix(name)) {
		return flashbackWriteOneWAL(inner, destDir, flashbackStripArchiveSuffix(name))
	}
	return nil, fmt.Errorf("不支持的增量包格式（既不是 tar 也不是 WAL 段名）: %s", filepath.Base(src))
}

func flashbackStripArchiveSuffix(name string) string {
	name = strings.TrimSpace(name)
	for _, suf := range []string{".tar.zst", ".tar.gz", ".tgz", ".zst", ".gz", ".tar"} {
		if strings.HasSuffix(strings.ToLower(name), suf) {
			return name[:len(name)-len(suf)]
		}
	}
	return name
}

func flashbackLooksLikeGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

func flashbackLooksLikeZstd(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

func flashbackLooksLikeTar(b []byte) bool {
	if len(b) < 262 {
		return false
	}
	magic := string(b[257:262])
	return magic == "ustar" || magic == "ustar\x00"
}

func flashbackExtractWALFromTar(r io.Reader, destDir string) ([]flashbackWALFile, error) {
	tr := tar.NewReader(r)
	var out []flashbackWALFile
	seen := map[string]struct{}{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeGNUSparse {
			continue
		}
		name := filepath.Base(hdr.Name)
		if !flashbackIsWALSegName(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		path := filepath.Join(destDir, name)
		n, err := flashbackWriteFileFromReader(tr, path)
		if err != nil {
			return out, fmt.Errorf("写出 %s: %w", name, err)
		}
		seen[name] = struct{}{}
		out = append(out, flashbackWALFile{Name: name, Size: n, Source: "cloud"})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("压缩包内没有标准 WAL 段（24 位十六进制文件名）")
	}
	return out, nil
}

func flashbackWriteOneWAL(r io.Reader, destDir, name string) ([]flashbackWALFile, error) {
	path := filepath.Join(destDir, name)
	n, err := flashbackWriteFileFromReader(r, path)
	if err != nil {
		return nil, err
	}
	return []flashbackWALFile{{Name: name, Size: n, Source: "cloud"}}, nil
}

func flashbackWriteFileFromReader(r io.Reader, path string) (int64, error) {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}
