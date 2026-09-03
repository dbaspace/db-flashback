package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	mdmmodel "db-flashback/internal/mdmmodel"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/databases/ent"
	"db-flashback/pkg/utils/log"
)

const (
	flashbackCheckPassed  = "passed"
	flashbackCheckFailed  = "failed"
	flashbackCheckWarning = "warning"
)

func flashbackAddCheck(items *[]dto.FlashbackCheckItem, code, name, status, msg string) {
	*items = append(*items, dto.FlashbackCheckItem{Code: code, Name: name, Status: status, Message: msg})
}

func flashbackAddTableScopeCheck(items *[]dto.FlashbackCheckItem, specified []string, wanted int) {
	names := flashbackNormalizeTableNames(specified)
	name := "多表"
	msg := fmt.Sprintf("多表 %d 项，命中 %d 张", len(names), wanted)
	switch {
	case len(names) == 0:
		name = "整库表"
		msg = fmt.Sprintf("整库 %d 张表", wanted)
	case len(names) == 1:
		name = "单表"
		msg = fmt.Sprintf("单表 %s，命中 %d 张", names[0], wanted)
	}
	flashbackAddCheck(items, "tables", name, flashbackCheckPassed, msg)
	if wanted >= flashbackAllTablesWarnMin {
		flashbackAddCheck(items, "table_count", "表数量", flashbackCheckWarning, msg+"，数量较多，解析时间可能较长")
	}
}

func flashbackValidateReq(req *dto.FlashbackTaskReq) error {
	if req == nil {
		return fmt.Errorf("request is nil")
	}
	if flashbackEngineIsPDU(req) {
		return flashbackValidatePDUReq(req)
	}
	if strings.TrimSpace(req.InstanceID) == "" {
		return fmt.Errorf("instance_id 必填")
	}
	if strings.TrimSpace(req.Database) == "" {
		return fmt.Errorf("database 必填")
	}
	req.Tables = flashbackNormalizeTableNames(req.Tables)
	for i, t := range req.Tables {
		if _, _, err := flashbackParseTableName(t); err != nil {
			return fmt.Errorf("tables[%d]: %w", i, err)
		}
	}
	if _, err := flashbackParseTime(req.TargetTime); err != nil {
		return fmt.Errorf("target_time: %w", err)
	}
	if strings.TrimSpace(req.EndTime) != "" {
		if _, err := flashbackParseTime(req.EndTime); err != nil {
			return fmt.Errorf("end_time: %w", err)
		}
	}
	kind := strings.ToLower(strings.TrimSpace(req.OutputKind))
	if kind != "" && kind != "flashback" && kind != "original" {
		return fmt.Errorf("output_kind 仅支持 flashback 或 original")
	}
	req.StartFile = strings.TrimSpace(req.StartFile)
	req.StopFile = strings.TrimSpace(req.StopFile)
	if err := flashbackValidateBinlogName(req.StartFile, "start_file"); err != nil {
		return err
	}
	if err := flashbackValidateBinlogName(req.StopFile, "stop_file"); err != nil {
		return err
	}
	if req.StartPos > 0 && req.StartFile == "" {
		return fmt.Errorf("指定 start_pos 时必须同时给出 start_file")
	}
	if req.StopPos > 0 && req.StopFile == "" {
		return fmt.Errorf("指定 stop_pos 时必须同时给出 stop_file")
	}
	if req.StartFile != "" && req.StopFile != "" {
		if flashbackMySQLFileLater(req.StartFile, req.StopFile) {
			return fmt.Errorf("start_file 不能晚于 stop_file")
		}
		startPos := flashbackMySQLNormalizeStartPos(req.StartPos)
		if req.StartFile == req.StopFile && req.StopPos > 0 && req.StopPos < startPos {
			return fmt.Errorf("同一 binlog 内 stop_pos 不能小于 start_pos")
		}
	}
	return nil
}

// flashbackWalLevelGate replica 允许（依赖 FPW）；minimal 拒绝。fpwOn 为 SHOW full_page_writes。
func flashbackWalLevelGate(walLevel string, fpwOn bool) (status, msg string) {
	lv := strings.ToLower(strings.TrimSpace(walLevel))
	switch lv {
	case "logical":
		return flashbackCheckPassed, walLevel
	case "replica":
		if !fpwOn {
			return flashbackCheckFailed, "当前 wal_level=replica 且 full_page_writes=off，DELETE/UPDATE 无法从 WAL 还原旧行。请开启 full_page_writes 或将 wal_level 设为 logical。"
		}
		return flashbackCheckWarning, "wal_level=replica：DELETE/UPDATE 旧行依赖 full_page_writes 页镜像，覆盖窗口内若无对应 FPW 则无法闪回。"
	case "":
		lv = "unknown"
	}
	if lv == "minimal" {
		return flashbackCheckFailed, "当前 wal_level=minimal，WAL 不含足够行级信息，无法闪回。"
	}
	return flashbackCheckFailed, fmt.Sprintf("当前 wal_level=%s，不满足闪回条件。", lv)
}

func flashbackConnectTarget(ctx context.Context, instanceID, dbName string) (*sql.DB, *ent.DomainInstance, *mdmmodel.ResourceDbsInfo, error) {
	inst, err := lookupConfiguredInstance(instanceID)
	if err != nil {
		return nil, nil, nil, err
	}
	dom := instanceToDomain(inst)
	res := instanceToResource(inst)
	switch {
	case flashbackIsMySQL(dom.DbType):
		db, _, err := openConfiguredMySQL(ctx, inst, dbName)
		if err != nil {
			return nil, dom, res, err
		}
		return db, dom, res, nil
	case flashbackIsPostgres(dom.DbType):
		db, err := openConfiguredPostgres(ctx, inst, dbName)
		if err != nil {
			return nil, dom, res, err
		}
		return db, dom, res, nil
	default:
		return nil, dom, nil, fmt.Errorf("仅支持 PostgreSQL / MySQL 实例，当前类型 %s", dom.DbType)
	}
}

func (s *FlashbackImpl) Precheck(c *gin.Context, req *dto.FlashbackTaskReq) (*dto.FlashbackPrecheckResult, error) {
	if err := flashbackValidateReq(req); err != nil {
		return nil, err
	}
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	if flashbackEngineIsPDU(req) {
		return s.flashbackPrecheckPDU(c.Request.Context(), req)
	}
	ctx := c.Request.Context()
	out := &dto.FlashbackPrecheckResult{Items: []dto.FlashbackCheckItem{}}
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

	db, dom, res, err := flashbackConnectTarget(ctx, req.InstanceID, req.Database)
	if err != nil {
		flashbackAddCheck(&out.Items, "connect", "连接目标库", flashbackCheckFailed, err.Error())
		out.OK = false
		return out, nil
	}
	defer db.Close()
	out.Host = strings.TrimSpace(dom.MainIP)
	if out.Host == "" {
		out.Host = dom.DomainName
	}
	out.Port = int(dom.Port)

	if flashbackIsMySQL(dom.DbType) {
		return flashbackPrecheckMySQL(ctx, db, req, out, target, end)
	}
	src := flashbackResolveWALSourceFromConn(res, dom, req.CloudInstanceID)
	cloudRoleHint := ""
	switch src.Kind {
	case flashbackWALSourceCloudOther:
		flashbackAddCheck(&out.Items, "wal_source", "WAL 来源", flashbackCheckFailed, src.Reason)
		out.OK = false
		return out, nil
	case flashbackWALSourceTencentNeedID:
		flashbackAddCheck(&out.Items, "cloud_instance", "云产品实例", flashbackCheckFailed, src.Reason)
		out.OK = false
		return out, nil
	case flashbackWALSourceCloudTencent:
		flashbackAddCheck(&out.Items, "wal_source", "WAL 来源", flashbackCheckPassed, src.Reason)
		out.ParseMode = flashbackParseModeCloud
	default:
		roles, rerr := flashbackListCloudRoles(ctx, db)
		if rerr != nil {
			log.Warn("flashback list cloud roles", zap.Error(rerr))
		} else {
			cloudRoleHint = flashbackCloudRoleReason(roles)
		}
		if cloudRoleHint != "" {
			flashbackAddCheck(&out.Items, "selfhosted", "自建实例", flashbackCheckWarning, cloudRoleHint+"；未命中云产品 ID，继续按自建探测。若实际是云库请在 MDM 打标签 flash_vendor 与 pg:postgres-xxxx")
		} else {
			flashbackAddCheck(&out.Items, "selfhosted", "自建实例", flashbackCheckPassed, src.Reason)
		}
		out.ParseMode = flashbackParseModeFile
	}

	var ver, walLevel, archiveMode, fpw string
	_ = db.QueryRowContext(ctx, `SHOW server_version`).Scan(&ver)
	_ = db.QueryRowContext(ctx, `SHOW wal_level`).Scan(&walLevel)
	_ = db.QueryRowContext(ctx, `SHOW archive_mode`).Scan(&archiveMode)
	_ = db.QueryRowContext(ctx, `SHOW full_page_writes`).Scan(&fpw)
	out.ServerVersion = ver
	out.WALLevel = walLevel
	out.ArchiveMode = archiveMode
	if out.ParseMode == "" {
		out.ParseMode = flashbackParseModeFile
	}
	if st, msg := flashbackVersionGate(ver); st == flashbackCheckFailed {
		flashbackAddCheck(&out.Items, "version", "PostgreSQL 版本", st, msg)
		out.OK = false
		return out, nil
	} else {
		flashbackAddCheck(&out.Items, "version", "PostgreSQL 版本", st, msg)
	}
	flashbackAddVersionImpacts(&out.Items, ver)
	fpwOn := strings.EqualFold(strings.TrimSpace(fpw), "on")
	if fpwOn {
		flashbackAddCheck(&out.Items, "full_page_writes", "full_page_writes", flashbackCheckPassed, "on")
	} else {
		st := flashbackCheckWarning
		if !strings.EqualFold(strings.TrimSpace(walLevel), "logical") {
			st = flashbackCheckFailed
		}
		flashbackAddCheck(&out.Items, "full_page_writes", "full_page_writes", st,
			fmt.Sprintf("当前 full_page_writes=%s。replica 级别闪回需要 on；logical 下仍建议开启以便 FPW 补齐旧行。", fpw))
		if st == flashbackCheckFailed {
			out.OK = false
			return out, nil
		}
	}
	if st, msg := flashbackWalLevelGate(walLevel, fpwOn); st == flashbackCheckFailed {
		flashbackAddCheck(&out.Items, "wal_level", "wal_level", st, msg)
		out.OK = false
		return out, nil
	} else {
		flashbackAddCheck(&out.Items, "wal_level", "wal_level", st, msg)
	}

	dict, err := flashbackLoadDictionary(ctx, db, req.Database, req.Tables)
	if err != nil {
		name := "指定表"
		if flashbackTablesIsAll(req.Tables) {
			name = "整库表"
		}
		flashbackAddCheck(&out.Items, "tables", name, flashbackCheckFailed, err.Error())
		out.OK = false
		return out, nil
	}
	var missing []string
	for _, rel := range dict.Wanted {
		if rel != nil && rel.Missing {
			missing = append(missing, rel.Schema+"."+rel.Name)
		}
	}
	if len(missing) > 0 {
		msg := "下列表当前不存在：" + strings.Join(missing, ", ")
		if flashbackWantDDL(req.SQLType) {
			flashbackAddCheck(&out.Items, "tables", "指定表", flashbackCheckWarning, msg+"。仅尝试从 WAL 目录还原 CREATE/DROP/ALTER，无法还原 DROP 后的行数据。")
		} else {
			flashbackAddCheck(&out.Items, "tables", "指定表", flashbackCheckFailed, msg+"。DML 闪回需要表仍存在；若要还原建表语句请将 sql_type 设为 ddl。")
			out.OK = false
			return out, nil
		}
	} else {
		flashbackAddTableScopeCheck(&out.Items, req.Tables, len(dict.Wanted))
	}
	var replWarn, typeWarn []string
	for _, rel := range dict.Wanted {
		if rel == nil {
			continue
		}
		switch rel.ReplIdent {
		case "n":
			replWarn = append(replWarn, rel.Schema+"."+rel.Name+" REPLICA IDENTITY NOTHING，UPDATE/DELETE 可能无旧行、需依赖 FPW")
		case "d":
			if len(rel.PKCols) == 0 {
				replWarn = append(replWarn, rel.Schema+"."+rel.Name+" 无主键且 REPLICA IDENTITY DEFAULT")
			}
		}
		_, uns := flashbackRelationTypeSummary(rel)
		typeWarn = append(typeWarn, uns...)
	}
	if len(replWarn) > 0 {
		flashbackAddCheck(&out.Items, "replica_identity", "REPLICA IDENTITY", flashbackCheckWarning, strings.Join(replWarn, "；"))
	} else {
		flashbackAddCheck(&out.Items, "replica_identity", "REPLICA IDENTITY", flashbackCheckPassed, "指定表可用于行还原")
	}
	if len(typeWarn) > 0 {
		flashbackAddCheck(&out.Items, "data_types", "列类型", flashbackCheckWarning, "以下列可能无法精确解码："+strings.Join(typeWarn, ", "))
	} else {
		flashbackAddCheck(&out.Items, "data_types", "列类型", flashbackCheckPassed, "指定表列类型均可按 PDU 清单解码")
	}
	flashbackAddWalMinerLimits(&out.Items, dict, req.SQLType)

	if src.Kind == flashbackWALSourceCloudTencent {
		return flashbackPrecheckPGCloud(ctx, db, out, target, end, src, res, req.CloudRegion)
	}

	live, err := flashbackListLiveWAL(ctx, db)
	if err != nil {
		flashbackAddCheck(&out.Items, "ls_wal", "pg_ls_waldir", flashbackCheckFailed, err.Error()+"。若实际是云库请在 MDM 打标签 pg:postgres-xxxx")
		out.OK = false
		return out, nil
	}
	if len(live) == 0 {
		flashbackAddCheck(&out.Items, "ls_wal", "pg_ls_waldir", flashbackCheckFailed, "pg_wal 无 WAL 文件")
		out.OK = false
		return out, nil
	}
	_, magic, err := flashbackProbeLiveWAL(ctx, db, live)
	if err != nil {
		msg := "无法读取 pg_wal 内容（需要超管或 pg_read_server_files）：" + err.Error()
		if cloudRoleHint != "" {
			msg += "；目标库存在云厂商角色，云托管实例通常禁止 SQL 读 WAL"
		}
		flashbackAddCheck(&out.Items, "read_wal", "pg_read_binary_file", flashbackCheckFailed, msg)
		out.OK = false
		return out, nil
	}
	if !flashbackLooksLikeWALMagic(magic) {
		flashbackAddCheck(&out.Items, "read_wal", "pg_read_binary_file", flashbackCheckFailed,
			fmt.Sprintf("已读到字节但不是 WAL 页头（magic=0x%04X）", magic))
		out.OK = false
		return out, nil
	}
	verHint := flashbackPageMagics[magic]
	if verHint == "" {
		verHint = "unknown"
	}
	readMsg := fmt.Sprintf("可远程读取 WAL 字节（magic=0x%04X / PG %s）", magic, verHint)
	if wm := flashbackMagicMajor(magic); wm > 0 {
		if sm := flashbackParseServerMajor(ver); sm > 0 && wm != sm {
			readMsg += fmt.Sprintf("；与 server_version 大版本 %d 不一致，请确认读到的是当前实例 WAL", sm)
		}
	}
	flashbackAddCheck(&out.Items, "read_wal", "pg_read_binary_file", flashbackCheckPassed, readMsg)

	archive, aerr := flashbackListArchiveWAL(flashbackArchiveDir(ctx))
	if aerr != nil {
		flashbackAddCheck(&out.Items, "archive", "归档目录", flashbackCheckWarning, aerr.Error())
	}
	merged := flashbackMergeWALFiles(live, archive)
	curWAL := flashbackCurrentWALName(ctx, db)
	merged = flashbackFilterCurrentTimeline(merged, curWAL)
	from, to := flashbackWALTimeSpan(merged)
	hasCurrent := flashbackLiveHasCurrent(live, curWAL)
	if hasCurrent {
		to = time.Now()
	}
	if !from.IsZero() {
		out.WALFrom = &from
		out.WALTo = &to
	}
	cpName, cpErr := flashbackCheckpointWALName(ctx, db)
	picked, bytes, truncated, cpOK := flashbackSelectWALPrecise(merged, target, end, flashbackMaxWALBytes(ctx), curWAL, cpName)
	if cpErr != nil {
		flashbackAddCheck(&out.Items, "checkpoint", "CHECKPOINT 段", flashbackCheckFailed,
			"无法读取 pg_control_checkpoint："+cpErr.Error())
		out.OK = false
		return out, nil
	}
	if !cpOK {
		flashbackAddCheck(&out.Items, "checkpoint", "CHECKPOINT 段", flashbackCheckFailed,
			fmt.Sprintf("所选 WAL 中找不到 checkpoint 段 %s，请补归档或提高体积上限（精准 FPW 需要从 checkpoint 开始攒页）", cpName))
		out.OK = false
		return out, nil
	}
	flashbackAddCheck(&out.Items, "checkpoint", "CHECKPOINT 段", flashbackCheckPassed,
		"将从 "+cpName+" 开始缓存 FPW")
	out.WALFiles = len(picked)
	out.WALBytes = bytes
	if gaps := flashbackWALContinuityGaps(picked); len(gaps) > 0 {
		flashbackAddCheck(&out.Items, "wal_continuity", "WAL 连续性", flashbackCheckWarning,
			"所选 WAL 段不连续（"+strings.Join(gaps, "；")+"），断档期间的变更无法还原")
	} else {
		flashbackAddCheck(&out.Items, "wal_continuity", "WAL 连续性", flashbackCheckPassed, "所选 WAL 段连续")
	}
	covered, covMsg := flashbackOnlineWALCoverage(target, end, to, hasCurrent)
	out.Covered = covered
	if !covered {
		flashbackAddCheck(&out.Items, "coverage", "WAL 时间覆盖", flashbackCheckFailed, covMsg)
		out.OK = false
		return out, nil
	}
	msg := fmt.Sprintf("%s。按 LSN 选段后按事务提交时间裁窗，将拉取 %d 个文件 / %d bytes（当前段 %s）", covMsg, len(picked), bytes, curWAL)
	if truncated {
		flashbackAddCheck(&out.Items, "coverage", "WAL 时间覆盖", flashbackCheckWarning, msg+"（超过体积上限，已截断）")
	} else {
		flashbackAddCheck(&out.Items, "coverage", "WAL 时间覆盖", flashbackCheckPassed, msg)
	}

	return flashbackPrecheckFinalize(out), nil
}

func flashbackPrecheckPGCloud(ctx context.Context, db *sql.DB, out *dto.FlashbackPrecheckResult, target, end time.Time, src flashbackWALSource, res *mdmmodel.ResourceDbsInfo, cloudRegion string) (*dto.FlashbackPrecheckResult, error) {
	flashbackAddCheck(&out.Items, "cloud_instance", "云产品实例", flashbackCheckPassed, src.InstanceID)
	if _, _, err := flashbackCloudVendorCreds(ctx, src.Vendor); err != nil {
		flashbackAddCheck(&out.Items, "cloud_creds", "云厂商凭证", flashbackCheckFailed, err.Error())
		return flashbackPrecheckFinalize(out), nil
	}
	idKey, keyKey, _, _, _ := flashbackCloudVendorKeyPair(src.Vendor)
	flashbackAddCheck(&out.Items, "cloud_creds", "云厂商凭证", flashbackCheckPassed,
		"已配置 "+idKey+" / "+keyKey)
	reg, rerr := flashbackResolveTencentRegion(ctx, res, cloudRegion)
	if rerr != nil {
		flashbackAddCheck(&out.Items, "cloud_region", "腾讯云 Region", flashbackCheckFailed, rerr.Error())
		return flashbackPrecheckFinalize(out), nil
	}
	flashbackAddCheck(&out.Items, "cloud_region", "腾讯云 Region", flashbackCheckPassed, reg.Region+"（来源="+reg.Reason+"）")

	cpTime := flashbackCheckpointTime(ctx, db)
	cpName, cpErr := flashbackCheckpointWALName(ctx, db)
	if cpErr != nil {
		flashbackAddCheck(&out.Items, "checkpoint", "CHECKPOINT 段", flashbackCheckWarning,
			"无法读取 pg_control_checkpoint，将按时间窗下载，执行时再校验："+cpErr.Error())
	} else {
		flashbackAddCheck(&out.Items, "checkpoint", "CHECKPOINT 段", flashbackCheckPassed,
			"期望从 "+cpName+" 开始缓存 FPW（解包后校验）")
	}
	from, to := flashbackCloudDownloadWindow(target, end, cpTime, flashbackCloudLookback(ctx))
	from, to = flashbackCloudApplyDownloadLag(from, to, flashbackCloudOSSLag)
	pkgs, err := flashbackListTencentLogBackups(ctx, src.InstanceID, reg.Region, from, to)
	if err != nil {
		flashbackAddCheck(&out.Items, "cloud_wal", "日志备份列表", flashbackCheckFailed, err.Error())
		return flashbackPrecheckFinalize(out), nil
	}
	if len(pkgs) == 0 {
		flashbackAddCheck(&out.Items, "cloud_wal", "日志备份列表", flashbackCheckFailed,
			fmt.Sprintf("时间窗 %s ~ %s 内没有 finished 的日志备份，请确认备份保留或调整时间",
				flashbackFormatLocalTime(from), flashbackFormatLocalTime(to)))
		return flashbackPrecheckFinalize(out), nil
	}
	total := flashbackCloudPkgsBytes(pkgs)
	spanFrom, spanTo := flashbackCloudPkgsSpan(pkgs)
	if !spanFrom.IsZero() {
		out.WALFrom = &spanFrom
		out.WALTo = &spanTo
	}
	out.WALFiles = len(pkgs)
	out.WALBytes = total
	maxPkgs := flashbackCloudMaxPackages(ctx)
	maxBytes := flashbackMaxWALBytes(ctx)
	if len(pkgs) > maxPkgs {
		flashbackAddCheck(&out.Items, "cloud_wal", "增量包数量", flashbackCheckFailed,
			fmt.Sprintf("时间窗内 %d 个增量包，超过上限 %d，请缩小 target_time～end_time", len(pkgs), maxPkgs))
		return flashbackPrecheckFinalize(out), nil
	}
	if maxBytes > 0 && total > maxBytes {
		flashbackAddCheck(&out.Items, "cloud_wal", "增量包体积", flashbackCheckFailed,
			fmt.Sprintf("日志备份合计 %d bytes，超过上限 %d，请缩小时间窗", total, maxBytes))
		return flashbackPrecheckFinalize(out), nil
	}
	msg := fmt.Sprintf("%d 个增量包 / %d bytes（%s ~ %s，云上传滞后：开始-3m 结束+3m），预检查只列举不下载体",
		len(pkgs), total, flashbackFormatLocalTime(from), flashbackFormatLocalTime(to))
	mbps := flashbackCloudDownloadMbps(ctx)
	if mbps > 0 && total > 0 {
		sec := float64(total) / (float64(mbps) * 1024 * 1024)
		msg += fmt.Sprintf("，按 %d MiB/s 约需 %.0f 秒", mbps, sec)
	}
	st := flashbackCheckPassed
	if len(pkgs) >= 12 {
		st = flashbackCheckWarning
	}
	flashbackAddCheck(&out.Items, "cloud_wal", "日志备份列表", st, msg)
	out.Covered = true
	return flashbackPrecheckFinalize(out), nil
}

func flashbackPrecheckFinalize(out *dto.FlashbackPrecheckResult) *dto.FlashbackPrecheckResult {
	out.OK = true
	for _, it := range out.Items {
		if it.Status == flashbackCheckFailed {
			out.OK = false
			break
		}
	}
	return out
}
