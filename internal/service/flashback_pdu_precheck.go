package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
)

func flashbackValidatePDUReq(req *dto.FlashbackTaskReq) error {
	if req == nil {
		return fmt.Errorf("request is nil")
	}
	req.Engine = flashback.EnginePDU
	req.PDUScene = flashbackNormalizePDUScene(req.PDUScene)
	req.ExportMode = flashbackNormalizeExportMode(req.ExportMode)
	req.PGDataPath = strings.TrimSpace(req.PGDataPath)
	req.ArchiveDest = strings.TrimSpace(req.ArchiveDest)
	req.DiskPath = strings.TrimSpace(req.DiskPath)
	req.Database = strings.TrimSpace(req.Database)
	if req.Database == "" {
		return fmt.Errorf("database 必填")
	}
	req.Tables = flashbackNormalizeTableNames(req.Tables)
	if _, err := flashbackParseTime(req.TargetTime); err != nil {
		return fmt.Errorf("target_time: %w", err)
	}
	if strings.TrimSpace(req.EndTime) != "" {
		if _, err := flashbackParseTime(req.EndTime); err != nil {
			return fmt.Errorf("end_time: %w", err)
		}
	}
	if req.PGDataPath == "" {
		return fmt.Errorf("pgdata_path 必填")
	}
	switch req.PDUScene {
	case flashbackPDUSceneWALDelete, flashbackPDUSceneWALUpdate:
		if req.ArchiveDest == "" {
			return fmt.Errorf("WAL 闪回需要 archive_dest")
		}
	case flashbackPDUSceneDrop:
		if req.DiskPath == "" {
			return fmt.Errorf("DROP 扫描需要 disk_path")
		}
		if flashbackTablesIsAll(req.Tables) {
			return fmt.Errorf("DROP 扫描必须指定表")
		}
	}
	return nil
}

func (s *FlashbackImpl) flashbackPrecheckPDU(ctx context.Context, req *dto.FlashbackTaskReq) (*dto.FlashbackPrecheckResult, error) {
	out := &dto.FlashbackPrecheckResult{Items: []dto.FlashbackCheckItem{}, ParseMode: flashbackParseModePDU}
	target, err := flashbackParseTime(req.TargetTime)
	if err != nil {
		return nil, err
	}
	end := time.Now()
	if strings.TrimSpace(req.EndTime) != "" {
		end, err = flashbackParseTime(req.EndTime)
		if err != nil {
			return nil, err
		}
	}
	if !end.After(target) {
		flashbackAddCheck(&out.Items, "time_range", "时间范围", flashbackCheckFailed, "end_time 必须晚于 target_time")
		out.OK = false
		return out, nil
	}
	flashbackAddCheck(&out.Items, "time_range", "时间范围", flashbackCheckPassed,
		fmt.Sprintf("%s → %s（按事务 COMMIT 时间裁窗）", flashbackFormatLocalTime(target), flashbackFormatLocalTime(end)))

	workRoot, _, _ := flashbackPDUWorkDirs(ctx, req.InstanceID, time.Now())
	flashbackAddCheck(&out.Items, "work_dir", "服务工作目录", flashbackCheckPassed,
		fmt.Sprintf("执行时按毫秒时间创建副本，例如 %s", workRoot))

	if id := strings.TrimSpace(req.InstanceID); id != "" {
		if db, dom, _, err := flashbackConnectTarget(ctx, id, req.Database); err == nil {
			out.Host = strings.TrimSpace(dom.MainIP)
			if out.Host == "" {
				out.Host = dom.DomainName
			}
			out.Port = int(dom.Port)
			_ = db.QueryRowContext(ctx, `SHOW data_directory`).Scan(new(string))
			db.Close()
			flashbackAddCheck(&out.Items, "connect", "实例（可选）", flashbackCheckPassed, "已连接，仅作建议；实际读本机路径")
		} else {
			flashbackAddCheck(&out.Items, "connect", "实例（可选）", flashbackCheckWarning, "连不上实例，继续纯离线: "+err.Error())
		}
	} else {
		flashbackAddCheck(&out.Items, "connect", "实例（可选）", flashbackCheckPassed, "未选实例，纯离线")
		out.Host = "local"
	}

	pgdata, err := flashbackPDUOpenDir(ctx, req.PGDataPath, "pgdata_path")
	if err != nil {
		if ssh, ok := flashbackPDULookupSSH(req.InstanceID); ok && !flashbackPDUHostIsLocal(ssh.Host) {
			if perr := flashbackPDUProbeSSH(ctx, ssh); perr != nil {
				flashbackAddCheck(&out.Items, "ssh", "SSH 互信", flashbackCheckFailed, perr.Error())
				flashbackAddCheck(&out.Items, "paths", "PGDATA", flashbackCheckFailed, err.Error())
				out.OK = false
				return flashbackPrecheckFinalize(out), nil
			}
			flashbackAddCheck(&out.Items, "ssh", "SSH 互信", flashbackCheckPassed,
				fmt.Sprintf("服务与实例不在同一台，执行时从 %s 拉取数据目录/WAL 到服务时间目录", ssh.spec()))
			flashbackAddCheck(&out.Items, "paths", "PGDATA", flashbackCheckPassed, "远程 "+req.PGDataPath+"，执行时拉取")
			if strings.TrimSpace(req.ArchiveDest) != "" {
				flashbackAddCheck(&out.Items, "wal_dir", "WAL 目录", flashbackCheckPassed, "远程 "+req.ArchiveDest+"，执行时拉取")
			}
			flashbackAddCheck(&out.Items, "catalog", "离线 catalog", flashbackCheckWarning, "远程模式，catalog 在拉取后校验")
			flashbackAddCheck(&out.Items, "source", "出处", flashbackCheckPassed,
				"离线解码参考 PDU-PostgreSQLDataUnloader（Apache-2.0）")
			return flashbackPrecheckFinalize(out), nil
		}
		if ssh, ok := flashbackPDULookupSSH(req.InstanceID); ok && flashbackPDUHostIsLocal(ssh.Host) {
			flashbackAddCheck(&out.Items, "ssh", "同机部署", flashbackCheckPassed, "服务与实例同机，不需要 SSH 互信")
		}
		flashbackAddCheck(&out.Items, "paths", "PGDATA", flashbackCheckFailed, err.Error())
		out.OK = false
		return flashbackPrecheckFinalize(out), nil
	}
	cat, err := flashbackOpenOfflinePGDATA(pgdata)
	if err != nil {
		flashbackAddCheck(&out.Items, "pg_version", "PGDATA", flashbackCheckFailed, err.Error())
		out.OK = false
		return flashbackPrecheckFinalize(out), nil
	}
	out.ServerVersion = cat.Version
	st, msg := flashbackPDUVersionGate(cat.Major)
	flashbackAddCheck(&out.Items, "pg_version", "PostgreSQL 版本", st, msg)
	if st == flashbackCheckFailed {
		out.OK = false
		return flashbackPrecheckFinalize(out), nil
	}

	if err := cat.useDatabase(req.Database); err != nil {
		flashbackAddCheck(&out.Items, "catalog", "离线 catalog", flashbackCheckFailed, err.Error())
		out.OK = false
		return flashbackPrecheckFinalize(out), nil
	}
	tables := cat.listUserTables()
	flashbackAddCheck(&out.Items, "catalog", "离线 catalog", flashbackCheckPassed,
		fmt.Sprintf("库 %s oid=%d，用户表 %d 张（%s）", cat.DBName, cat.DBOID, len(tables), flashbackFormatUserTables(tables, 8)))

	scene := flashbackNormalizePDUScene(req.PDUScene)
	if scene != flashbackPDUSceneDrop && flashbackTablesIsAll(req.Tables) && len(tables) == 0 {
		flashbackAddCheck(&out.Items, "tables", "整库表", flashbackCheckFailed,
			flashbackPDUDictMissError(cat, req.Tables, true).Error())
		out.OK = false
		return flashbackPrecheckFinalize(out), nil
	}
	if scene != flashbackPDUSceneDrop && !flashbackTablesIsAll(req.Tables) {
		dict, err := cat.loadDictionary(req.Tables)
		if err != nil {
			flashbackAddCheck(&out.Items, "tables", flashbackTableScopeName(req.Tables), flashbackCheckFailed, err.Error())
			out.OK = false
			return flashbackPrecheckFinalize(out), nil
		}
		flashbackAddTableScopeCheck(&out.Items, req.Tables, len(dict.Wanted))
	} else if scene != flashbackPDUSceneDrop {
		flashbackAddTableScopeCheck(&out.Items, req.Tables, len(tables))
	}

	if scene == flashbackPDUSceneWALDelete || scene == flashbackPDUSceneWALUpdate {
		if err := flashbackPDUPathAllowed(ctx, req.ArchiveDest); err != nil {
			flashbackAddCheck(&out.Items, "wal_dir", "WAL 目录", flashbackCheckFailed, err.Error())
			out.OK = false
			return flashbackPrecheckFinalize(out), nil
		}
		files, err := flashbackListArchiveWAL(req.ArchiveDest)
		if err != nil {
			flashbackAddCheck(&out.Items, "wal_dir", "WAL 目录", flashbackCheckFailed, err.Error())
			out.OK = false
			return flashbackPrecheckFinalize(out), nil
		}
		files = flashbackPDUFilterWALNames(files, req.StartWAL, req.EndWAL)
		if len(files) == 0 {
			flashbackAddCheck(&out.Items, "wal_dir", "WAL 目录", flashbackCheckFailed, "目录内没有 WAL 段")
			out.OK = false
			return flashbackPrecheckFinalize(out), nil
		}
		from, to := flashbackWALTimeSpan(files)
		out.WALFiles = len(files)
		out.WALFrom, out.WALTo = &from, &to
		for _, f := range files {
			out.WALBytes += f.Size
		}
		dirSt := flashbackCheckPassed
		dirMsg := flashbackPDUWALDirMessage(len(files), out.WALBytes, to)
		if len(files) > 250 {
			dirSt = flashbackCheckFailed
			dirMsg += "；超过 250 个段，请设置 start_wal/end_wal 或缩小时间窗"
		}
		flashbackAddCheck(&out.Items, "wal_dir", "WAL 目录", dirSt, dirMsg)
		covSt, covMsg, covered := flashbackPDUCoverage(target, end, to)
		out.Covered = covered
		if dirSt == flashbackCheckFailed {
			covSt = flashbackCheckFailed
		}
		flashbackAddCheck(&out.Items, "coverage", "时间覆盖", covSt, covMsg)
	}

	if scene == flashbackPDUSceneDrop {
		if err := flashbackPDUPathAllowed(ctx, req.DiskPath); err != nil {
			flashbackAddCheck(&out.Items, "disk", "扫描路径", flashbackCheckFailed, err.Error())
			out.OK = false
			return flashbackPrecheckFinalize(out), nil
		}
		if _, err := os.Stat(filepath.Clean(req.DiskPath)); err != nil {
			flashbackAddCheck(&out.Items, "disk", "扫描路径", flashbackCheckFailed, err.Error())
			out.OK = false
			return flashbackPrecheckFinalize(out), nil
		}
		flashbackAddCheck(&out.Items, "disk", "扫描路径", flashbackCheckPassed, "只读扫描 "+req.DiskPath)
	}

	flashbackAddCheck(&out.Items, "source", "出处", flashbackCheckPassed,
		"离线解码参考 PDU-PostgreSQLDataUnloader（Apache-2.0）")
	return flashbackPrecheckFinalize(out), nil
}

func flashbackFormatLocalTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(flashbackTimeLocation()).Format("2006-01-02 15:04:05Z07:00")
}

func flashbackOnlineWALCoverage(target, end, latest time.Time, hasCurrent bool) (covered bool, msg string) {
	if latest.IsZero() {
		return false, "没有可用的 WAL 段时间"
	}
	covered = !target.After(latest.Add(time.Hour))
	latestLabel := "最新段 " + flashbackFormatLocalTime(latest)
	if hasCurrent {
		latestLabel = "覆盖到当前写入 " + flashbackFormatLocalTime(latest)
	}
	msg = fmt.Sprintf("任务窗 %s ~ %s；%s。回收段更早的文件时间不是闪回起点",
		flashbackFormatLocalTime(target), flashbackFormatLocalTime(end), latestLabel)
	if !covered {
		msg += "；target_time 晚于 WAL 覆盖 " + flashbackFormatLocalTime(latest)
	}
	return covered, msg
}

func flashbackPDUWALDirMessage(n int, bytes int64, latest time.Time) string {
	msg := fmt.Sprintf("%d 个 WAL 段，约 %s", n, flashbackFormatBytes(bytes))
	if !latest.IsZero() {
		msg += "；最新段文件时间 " + flashbackFormatLocalTime(latest)
	}
	return msg + "。执行时解析副本内全部段，变更按 COMMIT 时间过滤"
}

func flashbackPDUCoverage(target, end, latest time.Time) (status, msg string, covered bool) {
	covered = !latest.IsZero() && !latest.Before(target)
	msg = fmt.Sprintf("任务窗 %s ~ %s；最新段 %s。目录里更早的文件时间是回收槽位，不是从那天开始闪回",
		flashbackFormatLocalTime(target), flashbackFormatLocalTime(end), flashbackFormatLocalTime(latest))
	status = flashbackCheckPassed
	if !covered {
		status = flashbackCheckWarning
		msg += "；最新段早于任务起始，这次 DML 可能还不在已刷盘的段里"
	}
	return status, msg, covered
}

func flashbackPDUVersionGate(major int) (string, string) {
	if major < 14 {
		return flashbackCheckFailed, fmt.Sprintf("PDU 离线模式支持 PostgreSQL 14–18，当前 %d", major)
	}
	if major > 18 {
		return flashbackCheckWarning, fmt.Sprintf("官方 PDU 文档覆盖到 18，当前 %d，将尽量按 18 布局解码", major)
	}
	return flashbackCheckPassed, fmt.Sprintf("PostgreSQL %d", major)
}

func flashbackPDUFilterWALNames(files []flashbackWALFile, start, end string) []flashbackWALFile {
	start = strings.TrimSpace(start)
	end = strings.TrimSpace(end)
	if start == "" && end == "" {
		return files
	}
	var out []flashbackWALFile
	for _, f := range files {
		if start != "" && f.Name < start {
			continue
		}
		if end != "" && f.Name > end {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (s *FlashbackImpl) DiscoverPDU(c *gin.Context, req *dto.FlashbackPDUDiscoverReq) (*dto.FlashbackPDUDiscoverResult, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	ctx := c.Request.Context()
	if err := s.ensureReady(ctx); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(req.InstanceID)
	out := &dto.FlashbackPDUDiscoverResult{OfflineRoot: flashbackPDUOfflineRoot(ctx), Source: "instance"}
	if id != "" {
		pgdata, wal, ver, err := flashbackProbeInstanceDirs(ctx, id, req.Database)
		if err != nil {
			out.Message = "实例路径探测失败: " + err.Error()
		} else {
			out.RemotePGData = pgdata
			out.RemoteWAL = wal
			out.PGDataPath = pgdata
			out.ArchiveDest = wal
			out.PGVersion = ver
		}
	}
	if out.PGDataPath == "" {
		out.PGDataPath = strings.TrimSpace(req.PGDataPath)
		out.Source = "form"
	}
	if out.ArchiveDest == "" && out.PGDataPath != "" {
		out.ArchiveDest = flashbackSuggestWALDir(out.PGDataPath)
	}
	if out.PGDataPath == "" {
		return &dto.FlashbackPDUDiscoverResult{OK: false, OfflineRoot: out.OfflineRoot, Message: "请先选择实例，或填写数据目录"}, nil
	}

	example := filepath.Join(flashbackPDUOfflineRoot(ctx), flashbackPDUStagingName(id), flashbackPDUWorkStamp(time.Now()))
	opened, err := flashbackPDUOpenDir(ctx, out.PGDataPath, "pgdata_path")
	if err != nil {
		out.OK = true
		msg := fmt.Sprintf("实例数据目录 %s，WAL %s。执行时按毫秒创建服务副本（同机本地复制；跨机经 SSH 互信 rsync），例如 %s", out.PGDataPath, out.ArchiveDest, example)
		if out.Message != "" {
			msg = out.Message + "。" + msg
		}
		out.Message = msg
		return out, nil
	}
	out.LocalOK = true
	if resolved := flashbackLocalPGWAL(opened); resolved != "" && out.ArchiveDest == "" {
		out.ArchiveDest = resolved
	}
	cat, err := flashbackOpenOfflinePGDATA(opened)
	if err != nil {
		out.OK = true
		out.Message = "服务目录已就绪，但还不是完整 PGDATA（缺 PG_VERSION 等），请拷入副本: " + err.Error()
		return out, nil
	}
	out.OK = true
	out.PGVersion = cat.Version
	dbs, err := cat.listDatabases()
	if err != nil {
		out.Message = err.Error()
		return out, nil
	}
	want := strings.TrimSpace(req.Database)
	for _, db := range dbs {
		item := dto.FlashbackPDUDiscoverDB{Name: db.Name, OID: db.OID}
		if want == "" || want == db.Name {
			if err := cat.useDatabase(db.Name); err == nil {
				grouped := map[string][]string{}
				for _, t := range cat.listUserTables() {
					grouped[t[0]] = append(grouped[t[0]], t[1])
				}
				for sch, tabs := range grouped {
					item.Schemas = append(item.Schemas, dto.FlashbackPDUDiscoverSchema{Name: sch, Tables: tabs})
				}
			}
		}
		out.Databases = append(out.Databases, item)
	}
	if out.Message == "" {
		out.Message = fmt.Sprintf("已探测实例数据目录 %s，WAL %s。执行时将复制到 %s/<时间ms>/",
			out.PGDataPath, out.ArchiveDest, filepath.Join(flashbackPDUOfflineRoot(ctx), flashbackPDUStagingName(id)))
	}
	return out, nil
}

func flashbackProbeInstanceDirs(ctx context.Context, instanceID, dbName string) (pgdata, wal, version string, err error) {
	db, _, _, err := flashbackConnectTarget(ctx, instanceID, dbName)
	if err != nil {
		return "", "", "", err
	}
	defer db.Close()
	var dataDir, archiveMode, archiveCmd string
	if err := db.QueryRowContext(ctx, `SHOW data_directory`).Scan(&dataDir); err != nil {
		return "", "", "", fmt.Errorf("SHOW data_directory: %w", err)
	}
	_ = db.QueryRowContext(ctx, `SHOW archive_mode`).Scan(&archiveMode)
	_ = db.QueryRowContext(ctx, `SHOW archive_command`).Scan(&archiveCmd)
	_ = db.QueryRowContext(ctx, `SHOW server_version`).Scan(&version)
	pgdata = filepath.Clean(strings.TrimSpace(dataDir))
	if pgdata == "" || pgdata == "." {
		return "", "", version, fmt.Errorf("实例未返回 data_directory（云库可能隐藏）")
	}
	archiveOn := archiveMode == "on" || archiveMode == "always"
	if dest := flashbackParseArchiveDest(archiveCmd); archiveOn && dest != "" {
		wal = dest
	} else if local := flashbackLocalPGWAL(pgdata); local != "" {
		wal = local
	} else {
		wal = filepath.Join(pgdata, "pg_wal")
	}
	return pgdata, wal, version, nil
}

func flashbackSuggestWALDir(pgdata string) string {
	if local := flashbackLocalPGWAL(pgdata); local != "" {
		return local
	}
	pgdata = filepath.Clean(strings.TrimSpace(pgdata))
	if pgdata == "" || pgdata == "." {
		return ""
	}
	return filepath.Join(pgdata, "pg_wal")
}

func flashbackLocalPGWAL(pgdata string) string {
	wal := filepath.Join(filepath.Clean(strings.TrimSpace(pgdata)), "pg_wal")
	if wal == "pg_wal" {
		return ""
	}
	fi, err := os.Lstat(wal)
	if err != nil {
		return ""
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		if real, err := filepath.EvalSymlinks(wal); err == nil && real != "" {
			return real
		}
	}
	return wal
}

func flashbackParseArchiveDest(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" || cmd == "(disabled)" {
		return ""
	}
	var best string
	for _, tok := range strings.Fields(cmd) {
		tok = strings.Trim(tok, `"'`)
		if tok == "" || strings.Contains(tok, "%p") {
			continue
		}
		slash := strings.Index(tok, "/")
		if slash < 0 {
			continue
		}
		path := tok[slash:]
		if strings.Contains(path, "%f") {
			path = filepath.Dir(filepath.Clean(strings.ReplaceAll(path, "%f", "X")))
		} else {
			path = filepath.Clean(path)
		}
		if path == "" || path == "." || path == "/" {
			continue
		}
		best = path
	}
	if best == "." {
		return ""
	}
	return best
}
