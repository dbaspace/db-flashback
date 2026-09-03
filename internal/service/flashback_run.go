package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"db-flashback/internal/storage/flashback"
	"db-flashback/pkg/utils/log"
)

func (s *FlashbackImpl) logf(ctx context.Context, taskID, level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := s.store.InsertLog(ctx, taskID, level, msg); err != nil {
		log.Warn("flashback insert log", zap.Error(err), zap.String("task_id", taskID))
	}
}

func flashbackClampStage(done, total int) int {
	if done < 0 {
		return 0
	}
	if total > 0 && done > total {
		return total
	}
	return done
}

func (s *FlashbackImpl) writeFlashbackProgress(ctx context.Context, taskID, workDir string, walBytes int64, walFiles, changeCount, logDone, logTotal, parseDone, parseTotal int) {
	_ = s.store.UpdateProgress(ctx, taskID, workDir, walBytes, walFiles, changeCount)
	_ = s.store.UpdateStageProgress(ctx, taskID, flashbackClampStage(logDone, logTotal), logTotal, flashbackClampStage(parseDone, parseTotal), parseTotal)
}

func (s *FlashbackImpl) writeFlashbackStage(ctx context.Context, taskID string, logDone, logTotal, parseDone, parseTotal int) {
	_ = s.store.UpdateStageProgress(ctx, taskID, flashbackClampStage(logDone, logTotal), logTotal, flashbackClampStage(parseDone, parseTotal), parseTotal)
}

func (s *FlashbackImpl) cleanupTaskWAL(ctx context.Context, taskID string) {
	if err := flashbackCleanupTaskDir(flashbackWorkDirBase(ctx), taskID); err != nil {
		s.logf(ctx, taskID, "WARN", "清理本地 WAL 失败: %v", err)
		log.Warn("flashback cleanup wal", zap.Error(err), zap.String("task_id", taskID))
		return
	}
	s.logf(ctx, taskID, "INFO", "已删除本地拉取的 WAL 文件")
}

func (s *FlashbackImpl) runTask(ctx context.Context, taskID string) {
	s.logf(ctx, taskID, "INFO", "排队等待全局闪回锁（同时只执行 1 个任务）")
	unlock, err := flashbackAcquireRunLock(ctx, flashbackLockWaitBounded(ctx, flashbackRunLockWait(ctx)))
	if err != nil {
		msg := fmt.Sprintf("等待闪回执行锁失败：%v", err)
		_ = s.store.UpdateStatus(ctx, taskID, flashback.StatusFailed, msg, "")
		s.logf(ctx, taskID, "ERROR", "%s", msg)
		log.Error("flashback run lock", zap.String("task_id", taskID), zap.Error(err))
		return
	}
	defer unlock()
	s.logf(ctx, taskID, "INFO", "已获得闪回执行锁，开始解析")

	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("panic: %v（进程未退出时已清理本地 WAL）", rec)
			_ = s.store.UpdateStatus(ctx, taskID, flashback.StatusFailed, msg, "")
			s.logf(ctx, taskID, "ERROR", "%s", msg)
			log.Error("flashback task panic", zap.String("task_id", taskID), zap.Any("panic", rec))
			s.cleanupTaskWAL(ctx, taskID)
		}
	}()
	if err := s.executeTask(ctx, taskID); err != nil {
		msg := flashbackPublicError(err)
		_ = s.store.UpdateStatus(ctx, taskID, flashback.StatusFailed, msg, "")
		s.logf(ctx, taskID, "ERROR", "%s", msg)
		log.Error("flashback task failed", zap.String("task_id", taskID), zap.Error(err))
	}
}

func (s *FlashbackImpl) executeTask(ctx context.Context, taskID string) error {
	row, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("task not found")
	}
	if err := s.store.UpdateStatus(ctx, taskID, flashback.StatusRunning, "", ""); err != nil {
		return err
	}
	if flashbackRowIsPDU(row) {
		s.logf(ctx, taskID, "INFO", "PDU 离线模式，不连接业务库")
		return s.executeTaskPDU(ctx, taskID, row)
	}
	s.logf(ctx, taskID, "INFO", "开始连接 %s:%d/%s", row.Host, row.Port, row.DatabaseName)

	db, dom, res, err := flashbackConnectTarget(ctx, row.InstanceID, row.DatabaseName)
	if err != nil {
		return err
	}
	defer db.Close()
	if dom != nil && flashbackIsMySQL(dom.DbType) {
		return s.executeTaskMySQL(ctx, taskID, row, db)
	}
	walSrc := flashbackResolveWALSourceFromConn(res, dom, "")
	s.logf(ctx, taskID, "INFO", "WAL来源=%s 原因=%s", walSrc.Kind, walSrc.Reason)
	switch walSrc.Kind {
	case flashbackWALSourceCloudOther, flashbackWALSourceTencentNeedID:
		return fmt.Errorf("%s", walSrc.Reason)
	}

	var tables []string
	_ = json.Unmarshal([]byte(row.Tables), &tables)
	dict, src, err := flashbackOpenTaskDictionary(ctx, db, taskID, row.DatabaseName, tables)
	if err != nil {
		return err
	}
	s.logf(ctx, taskID, "INFO", "已加载数据字典（%s，%d 张表，dboid=%d relfilenode=%d oid=%d）",
		src, len(dict.Wanted), dict.DBOID, firstRelNode(dict), firstOID(dict))
	for _, rel := range dict.Wanted {
		if rel == nil {
			continue
		}
		if rel.Missing {
			s.logf(ctx, taskID, "WARN", "表 %s.%s 当前不存在，仅尝试从 WAL 目录还原 DDL", rel.Schema, rel.Name)
			continue
		}
		if rel.OID != 0 && rel.RelNode != 0 && rel.OID != rel.RelNode {
			s.logf(ctx, taskID, "WARN", "表 %s.%s relfilenode(%d) 与 oid(%d) 不同，VACUUM FULL/TRUNCATE/改表空间之后、该 DDL 之前的 DML 无法解析",
				rel.Schema, rel.Name, rel.RelNode, rel.OID)
		}
	}
	if err := flashbackAttachCatalog(ctx, db, dict); err != nil {
		s.logf(ctx, taskID, "WARN", "加载系统表字典失败，DDL 与同事务建表后的 DML 将不可用: %v", err)
	} else {
		s.logf(ctx, taskID, "INFO", "已加载系统表字典（pg_class/pg_attribute/pg_namespace/pg_type）")
	}

	end := time.Now()
	if row.EndTime != nil {
		end = *row.EndTime
	}
	baseDir := flashbackWorkDirBase(ctx)
	workDir := filepath.Join(baseDir, taskID)
	walDir := filepath.Join(workDir, "wal")
	defer s.cleanupTaskWAL(ctx, taskID)
	s.writeFlashbackProgress(ctx, taskID, workDir, 0, 0, 0, 0, 0, 0, 0)

	var picked []flashbackWALFile
	var bytes int64
	var truncated bool
	walFiles := 0
	archiveDir := flashbackArchiveDir(ctx)
	if walSrc.Kind != flashbackWALSourceCloudTencent {
		live, err := flashbackListLiveWAL(ctx, db)
		if err != nil {
			return fmt.Errorf("%w。若实际是云库请在 MDM 打标签 flash_vendor 与 pg:postgres-xxxx", err)
		}
		archive, _ := flashbackListArchiveWAL(archiveDir)
		merged := flashbackMergeWALFiles(live, archive)
		curWAL := flashbackCurrentWALName(ctx, db)
		merged = flashbackFilterCurrentTimeline(merged, curWAL)
		cpName, cpErr := flashbackCheckpointWALName(ctx, db)
		if cpErr != nil {
			s.logf(ctx, taskID, "WARN", "读取 pg_control_checkpoint 失败: %v", cpErr)
		}
		var cpOK bool
		picked, bytes, truncated, cpOK = flashbackSelectWALPrecise(merged, row.TargetTime, end, flashbackMaxWALBytes(ctx), curWAL, cpName)
		if !cpOK {
			return fmt.Errorf("所选 WAL 中找不到 checkpoint 段 %s，请补归档或提高 WAL 体积上限后再解析", strings.TrimSpace(cpName))
		}
		if len(picked) == 0 {
			return fmt.Errorf("没有可拉取的 WAL 文件")
		}
		walFiles = len(picked)
		_ = s.store.UpdateStageProgress(ctx, taskID, 0, walFiles, 0, walFiles)
		s.logf(ctx, taskID, "INFO", "按 LSN 窗口准备拉取 %d 个 WAL 文件（%d bytes，当前段 %s），边拉边解析", len(picked), bytes, curWAL)
		if truncated {
			s.logf(ctx, taskID, "WARN", "WAL 体积超过上限，已从最旧段截断")
		}
	}

	debugKeep := os.Getenv("FLASHBACK_DEBUG_WAL") != ""
	maxSQLs := flashbackMaxSQLs(ctx)
	opFilter := flashbackNormalizeSQLTypes(row.SQLType)
	seq := 1
	var skipped, changeCount int
	sqlTrunc := false
	var flushErr error
	batch := make([]*flashback.SQLRow, 0, flashbackSQLInsertBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.store.InsertSQLs(ctx, batch); err != nil {
			return fmt.Errorf("写入平台闪回 SQL 失败：%w", err)
		}
		batch = batch[:0]
		return nil
	}
	handle := func(ch flashbackChange) bool {
		if row.StartXID > 0 && int64(ch.XID) < row.StartXID {
			return true
		}
		if row.StopXID > 0 && int64(ch.XID) > row.StopXID {
			return true
		}
		if !ch.TS.IsZero() {
			if !row.TargetTime.IsZero() && ch.TS.Before(row.TargetTime) {
				return true
			}
			if row.EndTime != nil && ch.TS.After(*row.EndTime) {
				return true
			}
		} else if !row.TargetTime.IsZero() || row.EndTime != nil {
			return true
		}
		if !flashbackWantOp(opFilter, ch.Op) {
			return true
		}
		if !dict.matchChange(ch) {
			return true
		}
		if strings.EqualFold(ch.Op, "DELETE") || strings.EqualFold(ch.Op, "UPDATE") {
			if flashbackValuesIncomplete(dict.match(ch.Schema, ch.Table), ch.Old) {
				flashbackFillOldFromDB(ctx, db, dict, &ch)
			}
		}
		if changeCount >= maxSQLs {
			sqlTrunc = true
			return false
		}
		undo, ur := flashbackUndoSQL(ch)
		redo, rr := flashbackRedoSQL(ch)
		var ts *time.Time
		if !ch.TS.IsZero() {
			t := ch.TS
			ts = &t
		}
		if undo != "" {
			batch = append(batch, &flashback.SQLRow{
				TaskID: taskID, Seq: seq, Kind: flashback.KindUndo,
				SchemaName: ch.Schema, TableName: ch.Table, Op: ch.Op,
				XID: int64(ch.XID), TS: ts, Statement: undo, Risk: ur,
			})
			seq++
			changeCount++
		} else {
			skipped++
		}
		if redo != "" {
			batch = append(batch, &flashback.SQLRow{
				TaskID: taskID, Seq: seq, Kind: flashback.KindRedo,
				SchemaName: ch.Schema, TableName: ch.Table, Op: ch.Op,
				XID: int64(ch.XID), TS: ts, Statement: redo, Risk: rr,
			})
			seq++
		}
		if len(batch) >= flashbackSQLInsertBatch {
			if err := flush(); err != nil {
				flushErr = err
				return false
			}
		}
		return changeCount < maxSQLs
	}

	s.logf(ctx, taskID, "INFO", "按事务提交时间裁窗 %s ~ %s（只输出已提交事务，未提交/回滚不生成 SQL）",
		row.TargetTime.Format(time.RFC3339), end.Format(time.RFC3339))
	parseOpts := flashbackParseOpts{
		MaxChanges:  maxSQLs,
		DeleteAfter: !debugKeep,
		MaxFPWPages: flashbackMaxFPWPages(ctx),
		TimeFrom:    row.TargetTime,
		TimeTo:      end,
	}
	var st flashbackParseStats
	var pulled int64
	if walSrc.Kind == flashbackWALSourceCloudTencent {
		cpTime := flashbackCheckpointTime(ctx, db)
		from, to := flashbackCloudDownloadWindow(row.TargetTime, end, cpTime, flashbackCloudLookback(ctx))
		from, to = flashbackCloudApplyDownloadLag(from, to, flashbackCloudOSSLag)
		if err := flashbackCloudWaitDownloadLag(ctx, end, flashbackCloudOSSLag); err != nil {
			return fmt.Errorf("等待云厂商 WAL 上传滞后: %w", err)
		}
		reg, err := flashbackResolveTencentRegion(ctx, res, "")
		if err != nil {
			return err
		}
		s.logf(ctx, taskID, "INFO", "腾讯云 Region=%s 来源=%s", reg.Region, reg.Reason)
		s.logf(ctx, taskID, "INFO", "云厂商 WAL 上传滞后，下载窗 %s ~ %s（开始-3m 结束+3m）",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
		provider, err := flashbackNewTencentWALProvider(ctx, reg.Region)
		if err != nil {
			return err
		}
		spec := flashbackCloudWALSpec{InstanceID: walSrc.InstanceID, From: from, To: to}
		pkgs, err := provider.ListByTime(ctx, spec)
		if err != nil {
			return err
		}
		if len(pkgs) == 0 {
			return fmt.Errorf("时间窗内没有可下载的日志备份")
		}
		if len(pkgs) > flashbackCloudMaxPackages(ctx) {
			return fmt.Errorf("增量包 %d 个超过上限 %d，请缩小时间窗", len(pkgs), flashbackCloudMaxPackages(ctx))
		}
		walFiles = len(pkgs)
		_ = s.store.UpdateStageProgress(ctx, taskID, 0, walFiles, 0, walFiles)
		s.logf(ctx, taskID, "INFO", "按时间窗准备下载 %d 个增量包（合计 %d bytes），串行限速边下边解析",
			len(pkgs), flashbackCloudPkgsBytes(pkgs))
		dlDir := filepath.Join(workDir, "cloud_dl")
		mbps := flashbackCloudDownloadMbps(ctx)
		var bps int64
		if mbps > 0 {
			bps = int64(mbps) * 1024 * 1024
		}
		st, pulled, err = flashbackStreamCloudWAL(ctx, provider, spec, pkgs, walDir, dlDir, dict, parseOpts,
			flashbackMaxWALBytes(ctx), bps, flashbackCloudPkgRetries(ctx),
			func(done, total int) {
				s.writeFlashbackStage(ctx, taskID, done, total, done-1, total)
			},
			func(i int, obj flashbackCloudWALObject, files int, written int64) {
				s.logf(ctx, taskID, "INFO", "增量包 %d/%d %s size=%d 已解出 %d 段（累计 %d bytes）",
					i, len(pkgs), firstNonEmpty(obj.Name, obj.ID), obj.Size, files, written)
				s.writeFlashbackProgress(ctx, taskID, workDir, written, i, changeCount, i, len(pkgs), i, len(pkgs))
			}, handle)
		if err != nil {
			return fmt.Errorf("下载/解析云 WAL: %w", err)
		}
	} else {
		var err error
		st, pulled, err = flashbackStreamWAL(ctx, db, walDir, archiveDir, picked, dict, dict.DBOID, parseOpts,
			func(done, totalFiles int) {
				s.writeFlashbackStage(ctx, taskID, done, totalFiles, done-1, totalFiles)
			},
			func(name string, n int64, done, totalFiles int, written int64) {
				s.logf(ctx, taskID, "INFO", "已拉取并解析 %s（%d bytes，%d/%d）", name, n, done, totalFiles)
				s.writeFlashbackProgress(ctx, taskID, workDir, written, done, changeCount, done, totalFiles, done, totalFiles)
			}, handle)
		if err != nil {
			return fmt.Errorf("拉取/解析 WAL: %w", err)
		}
	}
	bytes = pulled
	if flushErr != nil {
		return flushErr
	}
	if err := flush(); err != nil {
		return err
	}
	if debugKeep && st.Matched == 0 {
		debugDir := filepath.Join(os.TempDir(), "jupiter-flashback-debug")
		_ = os.RemoveAll(debugDir)
		_ = flashbackCopyDir(walDir, debugDir)
		s.logf(ctx, taskID, "INFO", "未命中表，已保留 WAL 副本 %s", debugDir)
	}
	s.logf(ctx, taskID, "INFO", "WAL 解析统计：%s", st.String())
	s.writeFlashbackProgress(ctx, taskID, "", bytes, walFiles, changeCount, walFiles, walFiles, walFiles, walFiles)

	warn := ""
	if truncated {
		warn = "WAL 体积超过上限，已截断拉取。"
	}
	if sqlTrunc || st.ChangeTrunc {
		warn += fmt.Sprintf("undo SQL 超过上限 %d，已截断。请缩小时间窗或拆表。", maxSQLs)
	}
	if st.MultiInsertRows >= flashbackBulkInsertWarnRows {
		warn += fmt.Sprintf("检测到大宗 COPY/MULTI_INSERT（约 %d 行），解析耗内存且可能已截断。", st.MultiInsertRows)
	}
	if changeCount == 0 {
		warn += "未能从 WAL 解析出行级变更。" + st.String() + "。"
		if st.DeleteNoOld > 0 {
			warn += fmt.Sprintf("已看到目标表 DELETE 但未能还原旧行（其中 FPW 未找到 %d）。replica 级别需要时间窗内有对应 full page image；logical 级别需 REPLICA IDENTITY 记录旧行。", st.FPWMiss)
		} else if st.Matched == 0 {
			warn += "未命中目标表 relfilenode，请确认表名与库名，或表在时间窗内发生过 rewrite。"
		} else {
			warn += "常见原因：目标时间窗内无该表 DML。"
		}
		s.logf(ctx, taskID, "WARN", "%s", warn)
	} else {
		s.logf(ctx, taskID, "INFO", "生成 undo %d 条（跳过 %d）", changeCount, skipped)
	}
	return s.store.UpdateStatus(ctx, taskID, flashback.StatusSucceeded, "", strings.TrimSpace(warn))
}
