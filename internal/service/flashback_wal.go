package service

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	flashbackDefaultMaxWALBytes = int64(1 << 30) // 1GiB
	flashbackDefaultMaxSQLs     = 100000
	flashbackDefaultFPWPages    = 512
	flashbackSQLInsertBatch     = 1000
	flashbackWALReadChunk       = 4 * 1024 * 1024 // 4MiB，减少 pg_read_binary_file 往返
	gaFlashbackMaxWALBytes      = "flashback_max_wal_bytes"
	gaFlashbackMaxSQLs          = "flashback_max_sqls"
	gaFlashbackMaxFPWPages      = "flashback_max_fpw_pages"
	gaFlashbackWorkDir          = "flashback_workdir"
	gaFlashbackArchiveDir       = "flashback_archive_dir"
)

type flashbackWALFile struct {
	Name         string
	Size         int64
	Modification time.Time
	Source       string // live | archive
}

func flashbackMaxWALBytes(ctx context.Context) int64 {
	n := getGlobalArgIntDefault(ctx, gaFlashbackMaxWALBytes, int(flashbackDefaultMaxWALBytes))
	if n <= 0 {
		return flashbackDefaultMaxWALBytes
	}
	return int64(n)
}

func flashbackMaxSQLs(ctx context.Context) int {
	n := getGlobalArgIntDefault(ctx, gaFlashbackMaxSQLs, flashbackDefaultMaxSQLs)
	if n <= 0 {
		return flashbackDefaultMaxSQLs
	}
	return n
}

func flashbackMaxFPWPages(ctx context.Context) int {
	n := getGlobalArgIntDefault(ctx, gaFlashbackMaxFPWPages, flashbackDefaultFPWPages)
	if n <= 0 {
		return flashbackDefaultFPWPages
	}
	return n
}

// flashbackCleanupOrphanWorkDirs 删除工作目录下全部任务子目录（启动回收用，闪回不能跨进程续跑）。
func flashbackCleanupOrphanWorkDirs(base string) (int, error) {
	base = filepath.Clean(strings.TrimSpace(base))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return 0, fmt.Errorf("invalid workdir base")
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if err := flashbackCleanupTaskDir(base, e.Name()); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

func flashbackWorkDirBase(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_WORKDIR")); v != "" {
		return v
	}
	return getGlobalArgStrDefault(ctx, gaFlashbackWorkDir, filepath.Join(os.TempDir(), "jupiter-flashback"))
}

// flashbackCleanupTaskDir 删除本次拉取到本地的 WAL 工作目录（不影响目标库 pg_wal / 归档源）。
func flashbackCleanupTaskDir(base, taskID string) error {
	base = filepath.Clean(strings.TrimSpace(base))
	taskID = strings.TrimSpace(taskID)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return fmt.Errorf("invalid workdir base")
	}
	if taskID == "" || strings.Contains(taskID, "..") || strings.ContainsAny(taskID, `/\`) {
		return fmt.Errorf("invalid task id")
	}
	dir := filepath.Join(base, taskID)
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("refuse to delete %s", dir)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	_ = os.RemoveAll(filepath.Join(dir, "wal"))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return os.RemoveAll(dir)
	}
	keepDict := false
	for _, e := range ents {
		name := e.Name()
		if name == flashbackDictFileName {
			keepDict = true
			continue
		}
		p := filepath.Join(dir, name)
		if e.IsDir() {
			if name == "wal" {
				continue
			}
			_ = os.RemoveAll(p)
			continue
		}
		if flashbackIsWALSegName(name) || strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(p)
		}
	}
	if keepDict {
		return nil
	}
	return os.RemoveAll(dir)
}

func flashbackArchiveDir(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_ARCHIVE_DIR")); v != "" {
		return v
	}
	return strings.TrimSpace(getGlobalArgStrDefault(ctx, gaFlashbackArchiveDir, ""))
}

func flashbackListLiveWAL(ctx context.Context, db *sql.DB) ([]flashbackWALFile, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, size, modification FROM pg_ls_waldir() ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("pg_ls_waldir: %w", err)
	}
	defer rows.Close()
	var out []flashbackWALFile
	for rows.Next() {
		var f flashbackWALFile
		if err := rows.Scan(&f.Name, &f.Size, &f.Modification); err != nil {
			return nil, err
		}
		f.Name = strings.TrimSpace(f.Name)
		if !flashbackIsWALSegName(f.Name) {
			continue
		}
		f.Source = "live"
		out = append(out, f)
	}
	return out, rows.Err()
}

func flashbackIsWALSegName(name string) bool {
	if len(name) != 24 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func flashbackWALSegIndex(name string) (tli uint32, idx uint64, ok bool) {
	if !flashbackIsWALSegName(name) {
		return 0, 0, false
	}
	t64, err1 := strconv.ParseUint(name[0:8], 16, 32)
	i64, err2 := strconv.ParseUint(name[8:24], 16, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uint32(t64), i64, true
}

func flashbackWALContinuityGaps(files []flashbackWALFile) []string {
	if len(files) < 2 {
		return nil
	}
	sorted := append([]flashbackWALFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var gaps []string
	prevTLI, prevIdx, prevOK := flashbackWALSegIndex(sorted[0].Name)
	for i := 1; i < len(sorted); i++ {
		tli, idx, ok := flashbackWALSegIndex(sorted[i].Name)
		if !ok || !prevOK {
			prevTLI, prevIdx, prevOK = tli, idx, ok
			continue
		}
		if tli != prevTLI {
			gaps = append(gaps, fmt.Sprintf("%s -> %s (timeline)", sorted[i-1].Name, sorted[i].Name))
		} else if idx > prevIdx+1 {
			gaps = append(gaps, fmt.Sprintf("%s -> %s (缺 %d 段)", sorted[i-1].Name, sorted[i].Name, idx-prevIdx-1))
		}
		prevTLI, prevIdx, prevOK = tli, idx, ok
	}
	return gaps
}

func flashbackFilterCurrentTimeline(files []flashbackWALFile, currentName string) []flashbackWALFile {
	tli, _, ok := flashbackWALSegIndex(currentName)
	if !ok || len(files) == 0 {
		return files
	}
	var out []flashbackWALFile
	for _, f := range files {
		ft, _, ok := flashbackWALSegIndex(f.Name)
		if ok && ft == tli {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return files
	}
	return out
}

func flashbackListArchiveWAL(dir string) ([]flashbackWALFile, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read archive dir: %w", err)
	}
	var out []flashbackWALFile
	for _, e := range ents {
		if e.IsDir() || !flashbackIsWALSegName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, flashbackWALFile{
			Name: e.Name(), Size: info.Size(), Modification: info.ModTime().UTC(), Source: "archive",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// flashbackListWorkWAL 列出工作副本 / 本机 WAL 目录，按本地 archive 文件拷贝（不可标 live，否则 db=nil 会走在线拉段 panic）。
func flashbackListWorkWAL(dir string) ([]flashbackWALFile, error) {
	return flashbackListArchiveWAL(dir)
}

// flashbackSelectPDUWAL 离线副本已按时间点拷好，按段名纳入全部段；超 maxBytes 时丢最旧。
// 时间窗只在解析后按 COMMIT 过滤，不用文件 mtime 裁段。
func flashbackSelectPDUWAL(files []flashbackWALFile, maxBytes int64) (picked []flashbackWALFile, total int64, truncated bool) {
	if len(files) == 0 {
		return nil, 0, false
	}
	sorted := append([]flashbackWALFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if maxBytes > 0 {
		var keep int64
		cut := 0
		for i := len(sorted) - 1; i >= 0; i-- {
			if keep+sorted[i].Size > maxBytes && keep > 0 {
				truncated = true
				cut = i + 1
				break
			}
			keep += sorted[i].Size
		}
		sorted = sorted[cut:]
	}
	for _, f := range sorted {
		picked = append(picked, f)
		total += f.Size
	}
	return picked, total, truncated
}

func flashbackPDUNoWALError(dir string, files []flashbackWALFile, from, to time.Time) error {
	if len(files) == 0 {
		return fmt.Errorf("WAL 目录内没有可解析的段（需要 24 位十六进制文件名）: %s", dir)
	}
	mtimeFrom, mtimeTo := flashbackWALTimeSpan(files)
	return fmt.Errorf("时间窗 %s ~ %s 未选中 WAL 段（目录 %d 个，最新段 %s，目录最早文件 %s 为回收槽位）",
		flashbackFormatLocalTime(from), flashbackFormatLocalTime(to), len(files),
		flashbackFormatLocalTime(mtimeTo), flashbackFormatLocalTime(mtimeFrom))
}

func flashbackWALTimeSpan(files []flashbackWALFile) (from, to time.Time) {
	for i, f := range files {
		if i == 0 || f.Modification.Before(from) {
			from = f.Modification
		}
		if i == 0 || f.Modification.After(to) {
			to = f.Modification
		}
	}
	return from, to
}

func flashbackCurrentWALName(ctx context.Context, db *sql.DB) string {
	var name string
	_ = db.QueryRowContext(ctx, `SELECT pg_walfile_name(pg_current_wal_lsn())`).Scan(&name)
	return strings.TrimSpace(name)
}

func flashbackLiveHasCurrent(live []flashbackWALFile, currentName string) bool {
	if currentName == "" {
		return false
	}
	for _, f := range live {
		if f.Name == currentName {
			return true
		}
	}
	return false
}

func flashbackInferCurrentWALName(files []flashbackWALFile) string {
	var name string
	for _, f := range files {
		if f.Source == "live" && (name == "" || f.Name > name) {
			name = f.Name
		}
	}
	if name == "" && len(files) > 0 {
		name = files[0].Name
		for _, f := range files[1:] {
			if f.Name > name {
				name = f.Name
			}
		}
	}
	return name
}

// flashbackSelectWAL 按 LSN（段名）裁剪：
//   - 不拉 current 之后的回收段
//   - 不拉时间窗之前的历史 live（mtime 对已写完的段可信）
//   - 当前段 mtime 可能很旧，始终纳入
//   - 多留一段更早的 segment，便于跨过第一个 checkpoint
//   - 超过 maxBytes 时丢最旧、留最新（闪回通常要最近的 DML）
func flashbackSelectWAL(files []flashbackWALFile, from, to time.Time, maxBytes int64, currentName string) (picked []flashbackWALFile, total int64, truncated bool) {
	if len(files) == 0 {
		return nil, 0, false
	}
	sorted := append([]flashbackWALFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if strings.TrimSpace(currentName) == "" {
		currentName = flashbackInferCurrentWALName(sorted)
	}

	var cands []flashbackWALFile
	for _, f := range sorted {
		if currentName != "" && f.Name > currentName {
			continue
		}
		if !to.IsZero() && f.Source == "archive" && f.Modification.After(to) {
			continue
		}
		cands = append(cands, f)
	}
	if len(cands) == 0 {
		return nil, 0, false
	}

	start := 0
	if !from.IsZero() {
		prev := -1
		for i, f := range cands {
			if currentName != "" && f.Name == currentName && f.Source == "live" {
				continue
			}
			if !f.Modification.IsZero() && f.Modification.Before(from) {
				prev = i
			}
		}
		if prev >= 0 {
			start = prev
		}
	}
	cands = cands[start:]

	if maxBytes > 0 {
		var keep int64
		cut := 0
		for i := len(cands) - 1; i >= 0; i-- {
			if keep+cands[i].Size > maxBytes && keep > 0 {
				truncated = true
				cut = i + 1
				break
			}
			keep += cands[i].Size
		}
		cands = cands[cut:]
	}
	for _, f := range cands {
		picked = append(picked, f)
		total += f.Size
	}
	return picked, total, truncated
}

// flashbackSelectWALPrecise 在时间窗选段基础上向前包含 checkpoint 段；找不到则 ok=false。
func flashbackSelectWALPrecise(files []flashbackWALFile, from, to time.Time, maxBytes int64, currentName, checkpointName string) (picked []flashbackWALFile, total int64, truncated bool, ok bool) {
	picked, total, truncated = flashbackSelectWAL(files, from, to, maxBytes, currentName)
	checkpointName = strings.TrimSpace(checkpointName)
	if checkpointName == "" {
		return picked, total, truncated, false
	}
	if len(picked) == 0 {
		return picked, total, truncated, false
	}
	for _, f := range picked {
		if f.Name == checkpointName {
			return picked, total, truncated, true
		}
	}
	sorted := append([]flashbackWALFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var extra []flashbackWALFile
	found := false
	first := picked[0].Name
	for _, f := range sorted {
		if currentName != "" && f.Name > currentName {
			break
		}
		if f.Name == checkpointName {
			found = true
		}
		if found && f.Name < first {
			extra = append(extra, f)
		}
		if f.Name >= first {
			break
		}
	}
	if !found {
		return picked, total, truncated, false
	}
	var extraBytes int64
	for _, f := range extra {
		extraBytes += f.Size
	}
	if maxBytes > 0 && total+extraBytes > maxBytes {
		return picked, total, true, false
	}
	picked = append(extra, picked...)
	total += extraBytes
	return picked, total, truncated, true
}

func flashbackCheckpointWALName(ctx context.Context, db *sql.DB) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `SELECT pg_walfile_name(redo_lsn) FROM pg_control_checkpoint()`).Scan(&name)
	if err != nil {
		err = db.QueryRowContext(ctx, `SELECT pg_walfile_name(checkpoint_lsn) FROM pg_control_checkpoint()`).Scan(&name)
	}
	return strings.TrimSpace(name), err
}

func flashbackFetchLiveWAL(ctx context.Context, db *sql.DB, destDir string, files []flashbackWALFile) (int64, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return 0, err
	}
	var written int64
	for _, f := range files {
		if f.Source != "live" {
			continue
		}
		n, err := flashbackCopyWALFile(ctx, db, destDir, f)
		if err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func flashbackCopyWALFile(ctx context.Context, db *sql.DB, destDir string, f flashbackWALFile) (int64, error) {
	path := filepath.Join(destDir, f.Name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	var off int64
	remain := f.Size
	for remain > 0 {
		chunk := int64(flashbackWALReadChunk)
		if chunk > remain {
			chunk = remain
		}
		var raw []byte
		q := `SELECT pg_read_binary_file($1, $2, $3)`
		if err := db.QueryRowContext(ctx, q, "pg_wal/"+f.Name, off, chunk).Scan(&raw); err != nil {
			return off, fmt.Errorf("pg_read_binary_file %s offset %d: %w", f.Name, off, err)
		}
		if len(raw) == 0 {
			break
		}
		if _, err := out.Write(raw); err != nil {
			return off, err
		}
		n := int64(len(raw))
		off += n
		remain -= n
		if n < chunk {
			break
		}
	}
	return off, nil
}

func flashbackCopyFileStream(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

func flashbackCopyArchiveWAL(destDir string, archiveDir string, files []flashbackWALFile) (int64, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return 0, err
	}
	var written int64
	for _, f := range files {
		if f.Source != "archive" {
			continue
		}
		src := filepath.Join(archiveDir, f.Name)
		dst := filepath.Join(destDir, f.Name)
		n, err := flashbackCopyFileStream(src, dst)
		if err != nil {
			return written, fmt.Errorf("copy archive %s: %w", f.Name, err)
		}
		written += n
	}
	return written, nil
}

func flashbackMaterializeWAL(ctx context.Context, db *sql.DB, destDir, archiveDir string, f flashbackWALFile) (string, int64, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", 0, err
	}
	path := filepath.Join(destDir, f.Name)
	copyLocal := func() (string, int64, error) {
		n, err := flashbackCopyFileStream(filepath.Join(archiveDir, f.Name), path)
		return path, n, err
	}
	if db == nil && f.Source != "cloud" && f.Source != "local" {
		return copyLocal()
	}
	switch f.Source {
	case "archive":
		return copyLocal()
	case "cloud", "local":
		st, err := os.Stat(path)
		if err != nil {
			return "", 0, err
		}
		return path, st.Size(), nil
	default:
		n, err := flashbackCopyWALFile(ctx, db, destDir, f)
		return path, n, err
	}
}

// flashbackStreamWAL 拉一段、解析一段、按需删除，共享 parser（FPW / 跨段 continuation）。
func flashbackStreamWAL(
	ctx context.Context,
	db *sql.DB,
	destDir, archiveDir string,
	files []flashbackWALFile,
	dict *flashbackDictionary,
	dboid uint32,
	opts flashbackParseOpts,
	onFetch func(done, total int),
	onFile func(name string, n int64, done, total int, written int64),
	emit func(flashbackChange) bool,
) (flashbackParseStats, int64, error) {
	var st flashbackParseStats
	if dict != nil {
		st.WantedDB = dict.DBOID
		for _, rel := range dict.Wanted {
			if rel.RelNode != 0 {
				st.WantedRel = rel.RelNode
				break
			}
		}
	}
	p := &flashbackWALParser{
		dict: dict, dboid: dboid, fpw: flashbackNewFPWCache(opts.MaxFPWPages), st: &st,
		maxChanges: opts.MaxChanges, timeFrom: opts.TimeFrom, timeTo: opts.TimeTo,
		txn: flashbackNewTxnBuf(),
	}
	var written int64
	for i, f := range files {
		if p.maxChanges > 0 && p.emitted >= p.maxChanges {
			st.ChangeTrunc = true
			break
		}
		if p.pastEnd {
			break
		}
		path, n, err := flashbackMaterializeWAL(ctx, db, destDir, archiveDir, f)
		if err != nil {
			return st, written, fmt.Errorf("%s: %w", f.Name, err)
		}
		written += n
		if onFetch != nil {
			onFetch(i+1, len(files))
		}
		ch, err := p.feedFile(path)
		if opts.DeleteAfter {
			_ = os.Remove(path)
		}
		if err != nil {
			return st, written, fmt.Errorf("%s: %w", f.Name, err)
		}
		if onFile != nil {
			onFile(f.Name, n, i+1, len(files), written)
		}
		for _, c := range ch {
			if emit != nil && !emit(c) {
				st.ChangeTrunc = true
				return st, written, nil
			}
		}
	}
	flashbackFinishParser(p, opts, emit, &st)
	return st, written, nil
}

func flashbackMergeWALFiles(live, archive []flashbackWALFile) []flashbackWALFile {
	seen := map[string]flashbackWALFile{}
	for _, f := range archive {
		seen[f.Name] = f
	}
	for _, f := range live {
		seen[f.Name] = f // live 覆盖同名
	}
	out := make([]flashbackWALFile, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func flashbackWALProbeOrder(live []flashbackWALFile, current string) []string {
	seen := map[string]struct{}{}
	var names []string
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	add(current)
	for i := len(live) - 1; i >= 0; i-- {
		add(live[i].Name)
	}
	return names
}

func flashbackProbeLiveWAL(ctx context.Context, db *sql.DB, live []flashbackWALFile) (name string, magic uint16, err error) {
	cur := flashbackCurrentWALName(ctx, db)
	var lastErr error
	var lastName string
	var lastMagic uint16
	for _, n := range flashbackWALProbeOrder(live, cur) {
		m, e := flashbackProbeReadWAL(ctx, db, n)
		if e != nil {
			lastErr = e
			continue
		}
		lastName, lastMagic = n, m
		if flashbackLooksLikeWALMagic(m) {
			return n, m, nil
		}
	}
	if lastName != "" {
		return lastName, lastMagic, nil
	}
	if lastErr != nil {
		return "", 0, lastErr
	}
	return "", 0, fmt.Errorf("no wal file to probe")
}

func flashbackProbeReadWAL(ctx context.Context, db *sql.DB, name string) (uint16, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT pg_read_binary_file($1, 0, 64)`, "pg_wal/"+name).Scan(&raw)
	if err != nil {
		return 0, err
	}
	if len(raw) < 2 {
		return 0, fmt.Errorf("empty read")
	}
	return binary.LittleEndian.Uint16(raw[0:2]), nil
}

var flashbackTimeLoc = sync.OnceValue(func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil || loc == nil {
		return time.Local
	}
	return loc
})

func flashbackTimeLocation() *time.Location {
	return flashbackTimeLoc()
}

func flashbackParseTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	loc := flashbackTimeLocation()
	layouts := []string{
		time.RFC3339, time.RFC3339Nano,
		"2006-01-02 15:04:05", "2006-01-02T15:04:05",
		"2006-01-02 15:04:05Z07:00",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, raw, loc); err == nil {
			return t, nil
		}
		if t, err := time.Parse(l, raw); err == nil {
			return t, nil
		}
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(unix, 0), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", raw)
}
