package service

// PDU 离线闪回：参考 PDU-PostgreSQLDataUnloader（Apache-2.0）
// https://github.com/wublabdubdub/PDU-PostgreSQLDataUnloader
// 本文件实现引擎判断、路径白名单与任务 extra，不调用官方 pdu 二进制。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
)

const (
	flashbackParseModePDU = "pdu"

	flashbackPDUSceneWALDelete = "wal_delete"
	flashbackPDUSceneWALUpdate = "wal_update"
	flashbackPDUSceneUnload    = "unload"
	flashbackPDUSceneDrop      = "drop_table"

	flashbackPDULocalInstance = "pdu-local"
)

type flashbackPDUExtra struct {
	Scene         string `json:"scene,omitempty"`
	PGDataPath    string `json:"pgdata_path,omitempty"`
	ArchiveDest   string `json:"archive_dest,omitempty"`
	DiskPath      string `json:"disk_path,omitempty"`
	PGDataExclude string `json:"pgdata_exclude,omitempty"`
	StartWAL      string `json:"start_wal,omitempty"`
	EndWAL        string `json:"end_wal,omitempty"`
	ResMode       string `json:"resmode,omitempty"`
	ExportMode    string `json:"export_mode,omitempty"`
	IncludeDead   bool   `json:"include_dead,omitempty"`
}

func flashbackEngineIsPDU(req *dto.FlashbackTaskReq) bool {
	if req == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Engine), flashback.EnginePDU)
}

func flashbackRowIsPDU(row *flashback.TaskRow) bool {
	return row != nil && strings.EqualFold(strings.TrimSpace(row.Engine), flashback.EnginePDU)
}

func flashbackNormalizePDUScene(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case flashbackPDUSceneWALUpdate:
		return flashbackPDUSceneWALUpdate
	case flashbackPDUSceneUnload:
		return flashbackPDUSceneUnload
	case flashbackPDUSceneDrop:
		return flashbackPDUSceneDrop
	default:
		return flashbackPDUSceneWALDelete
	}
}

func flashbackNormalizeExportMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "csv":
		return "csv"
	case "both":
		return "both"
	default:
		return "sql"
	}
}

func flashbackPDUExtraFromReq(req *dto.FlashbackTaskReq) flashbackPDUExtra {
	if req == nil {
		return flashbackPDUExtra{}
	}
	return flashbackPDUExtra{
		Scene:         flashbackNormalizePDUScene(req.PDUScene),
		PGDataPath:    strings.TrimSpace(req.PGDataPath),
		ArchiveDest:   strings.TrimSpace(req.ArchiveDest),
		DiskPath:      strings.TrimSpace(req.DiskPath),
		PGDataExclude: strings.TrimSpace(req.PGDataExclude),
		StartWAL:      strings.TrimSpace(req.StartWAL),
		EndWAL:        strings.TrimSpace(req.EndWAL),
		ResMode:       strings.ToLower(strings.TrimSpace(req.PDUResMode)),
		ExportMode:    flashbackNormalizeExportMode(req.ExportMode),
		IncludeDead:   req.IncludeDead,
	}
}

func flashbackPDUExtraFromRow(row *flashback.TaskRow) flashbackPDUExtra {
	out := flashbackPDUExtra{}
	if row == nil || strings.TrimSpace(row.Extra) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(row.Extra), &out)
	out.Scene = flashbackNormalizePDUScene(out.Scene)
	out.ExportMode = flashbackNormalizeExportMode(out.ExportMode)
	return out
}

func flashbackMarshalPDUExtra(ex flashbackPDUExtra) string {
	raw, err := json.Marshal(ex)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func flashbackPDUAllowPaths(ctx context.Context) []string {
	var out []string
	if cfg := runtimeConfig(); cfg != nil {
		out = append(out, cfg.Flashback.OfflineAllowPaths...)
		if v := strings.TrimSpace(cfg.Flashback.WorkDir); v != "" {
			out = append(out, v)
		}
		if v := strings.TrimSpace(cfg.Flashback.ArchiveDir); v != "" {
			out = append(out, v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_WORKDIR")); v != "" {
		out = append(out, v)
	}
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_ARCHIVE_DIR")); v != "" {
		out = append(out, v)
	}
	if v := flashbackPDUOfflineRoot(ctx); v != "" {
		out = append(out, v)
	}
	out = append(out, "/tmp", "/data", "/var/lib/postgresql", "/home", "/Users")
	seen := map[string]bool{}
	var uniq []string
	for _, p := range out {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	_ = ctx
	return uniq
}

func flashbackPDUPathAllowed(ctx context.Context, raw string) error {
	path := filepath.Clean(strings.TrimSpace(raw))
	if path == "" {
		return fmt.Errorf("路径为空")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("必须是绝对路径: %s", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("路径非法: %s", path)
	}
	for _, root := range flashbackPDUAllowPaths(ctx) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || !strings.HasPrefix(rel, "..") {
			return nil
		}
	}
	return fmt.Errorf("路径不在 offline_allow_paths 白名单内: %s", path)
}

func flashbackPDUOfflineRoot(ctx context.Context) string {
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_OFFLINE_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	if cfg := runtimeConfig(); cfg != nil {
		if v := strings.TrimSpace(cfg.Flashback.OfflineRoot); v != "" {
			return filepath.Clean(v)
		}
	}
	return filepath.Join(flashbackWorkDirBase(ctx), "offline")
}

func flashbackPDUStagingName(instanceID string) string {
	id := strings.TrimSpace(instanceID)
	if id == "" {
		return "local"
	}
	var b strings.Builder
	for _, r := range id {
		if r == '.' || r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	s := strings.Trim(b.String(), ".")
	if s == "" || s == "." || s == ".." {
		return "local"
	}
	return s
}

func flashbackPDUWorkStamp(t time.Time) string {
	return t.Format("20060102150405.000")
}

func flashbackPDUWorkDirs(ctx context.Context, instanceID string, t time.Time) (root, pgdata, wal string) {
	root = filepath.Join(flashbackPDUOfflineRoot(ctx), flashbackPDUStagingName(instanceID), flashbackPDUWorkStamp(t))
	return root, filepath.Join(root, "pgdata"), filepath.Join(root, "pg_wal")
}

func flashbackPDUPrepareWorkCopy(ctx context.Context, instanceID, srcPG, srcWAL string) (pgdata, wal string, err error) {
	srcPG = strings.TrimSpace(srcPG)
	srcWAL = strings.TrimSpace(srcWAL)
	_, dstPG, dstWAL := flashbackPDUWorkDirs(ctx, instanceID, time.Now())
	if err := os.MkdirAll(dstPG, 0o700); err != nil {
		return "", "", err
	}
	if err := flashbackPDUFetchDir(ctx, instanceID, srcPG, dstPG, []string{"pg_wal"}, "pgdata_path"); err != nil {
		return "", "", err
	}
	walSrc := srcWAL
	if walSrc == "" {
		walSrc = flashbackSuggestWALDir(srcPG)
	}
	if walSrc != "" {
		if err := flashbackPDUFetchDir(ctx, instanceID, walSrc, dstWAL, nil, "archive_dest"); err != nil {
			return "", "", err
		}
		return dstPG, dstWAL, nil
	}
	return dstPG, dstWAL, nil
}

func flashbackPDUFetchDir(ctx context.Context, instanceID, src, dst string, excludes []string, name string) error {
	if opened, err := flashbackPDUOpenDir(ctx, src, name); err == nil {
		skip := map[string]bool{}
		for _, e := range excludes {
			skip[e] = true
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		if err := flashbackPDUCopyDir(opened, dst, skip); err != nil {
			return fmt.Errorf("本机复制 %s → %s: %w", name, dst, err)
		}
		return nil
	}
	ssh, ok := flashbackPDULookupSSH(instanceID)
	if !ok {
		return fmt.Errorf("%s 本机不可读，且未选实例，无法远程拉取", name)
	}
	if flashbackPDUHostIsLocal(ssh.Host) {
		return fmt.Errorf("%s 本机不可读（服务与实例同机，不走 SSH）: %s", name, src)
	}
	if err := flashbackPDURSyncRemote(ctx, ssh, src, dst, excludes); err != nil {
		return fmt.Errorf("%s 远程拉取失败，请确认服务与 %s 已 SSH 互信: %w", name, ssh.spec(), err)
	}
	return nil
}

func flashbackPDUCopyDir(src, dst string, skip map[string]bool) error {
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		if skip[top] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return flashbackPDUCopyFile(path, target)
	})
}

func flashbackPDUCopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	info, statErr := in.Stat()
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if statErr == nil {
		mtime := info.ModTime()
		_ = os.Chtimes(dst, mtime, mtime)
	}
	return nil
}

func flashbackPDUOpenDir(ctx context.Context, raw, name string) (string, error) {
	if err := flashbackPDUPathAllowed(ctx, raw); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	path := filepath.Clean(strings.TrimSpace(raw))
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录: %s", name, path)
	}
	return path, nil
}

func flashbackApplyPDUExtraToTask(t *dto.FlashbackTask, row *flashback.TaskRow) {
	if t == nil || !flashbackRowIsPDU(row) {
		return
	}
	ex := flashbackPDUExtraFromRow(row)
	t.Engine = flashback.EnginePDU
	t.PDUScene = ex.Scene
	t.PGDataPath = ex.PGDataPath
	t.ArchiveDest = ex.ArchiveDest
	t.DiskPath = ex.DiskPath
	t.ExportMode = ex.ExportMode
}
