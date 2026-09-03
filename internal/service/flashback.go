package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"go.uber.org/zap"

	"db-flashback/internal/config"
	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/flashback"
	"db-flashback/pkg/utils/log"
)

var (
	flashbackStore         = flashback.Store{}
	flashbackBootstrapOnce sync.Once
	flashbackBootstrapErr  error
)

type FlashbackImpl struct {
	store flashback.Store
}

func NewFlashbackImpl() *FlashbackImpl {
	return &FlashbackImpl{store: flashbackStore}
}

func instanceViewFromConfig(inst config.InstanceConfig, source string) dto.FlashbackInstanceView {
	port := inst.Port
	if port <= 0 {
		if flashbackIsMySQL(inst.DBType) {
			port = defaultMySQLPort
		} else {
			port = 5432
		}
	}
	return dto.FlashbackInstanceView{
		ID: strings.TrimSpace(inst.ID), DBType: normalizeInstanceDBType(inst.DBType),
		Host: strings.TrimSpace(inst.Host), Port: port, User: strings.TrimSpace(inst.User),
		Vendor: strings.TrimSpace(inst.Vendor), CloudInstanceID: strings.TrimSpace(inst.CloudInstanceID),
		Region: strings.TrimSpace(inst.Region), SSHUser: strings.TrimSpace(inst.SSHUser), SSHPort: inst.SSHPort,
		Source: source, HasPassword: strings.TrimSpace(inst.Password) != "",
	}
}

func instanceViewFromRow(r flashback.InstanceRow) dto.FlashbackInstanceView {
	return instanceViewFromConfig(instanceRowToConfig(r), "db")
}

// ListInstances 数据库地址优先，其次 YAML。任务只引用 id。
func (s *FlashbackImpl) ListInstances(ctx context.Context) []dto.FlashbackInstanceView {
	seen := map[string]dto.FlashbackInstanceView{}
	if rows, err := s.store.ListInstances(ctx); err == nil {
		for _, r := range rows {
			if strings.TrimSpace(r.ID) == "" {
				continue
			}
			seen[r.ID] = instanceViewFromRow(r)
		}
	}
	if cfg := runtimeConfig(); cfg != nil {
		for _, inst := range cfg.Instances {
			id := strings.TrimSpace(inst.ID)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = instanceViewFromConfig(inst, "yaml")
		}
	}
	out := make([]dto.FlashbackInstanceView, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *FlashbackImpl) SaveInstance(ctx context.Context, req *dto.FlashbackInstanceSave) (*dto.FlashbackInstanceView, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为空")
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return nil, fmt.Errorf("id 必填")
	}
	if strings.TrimSpace(req.Host) == "" {
		return nil, fmt.Errorf("host 必填")
	}
	row := flashback.InstanceRow{
		ID: id, DBType: strings.TrimSpace(req.DBType), Host: strings.TrimSpace(req.Host),
		Port: req.Port, User: strings.TrimSpace(req.User), Password: req.Password,
		SSLMode: strings.TrimSpace(req.SSLMode), Vendor: strings.TrimSpace(req.Vendor),
		CloudInstanceID: strings.TrimSpace(req.CloudInstanceID), Region: strings.TrimSpace(req.Region),
		Remark: strings.TrimSpace(req.Remark), SSHUser: strings.TrimSpace(req.SSHUser), SSHPort: req.SSHPort,
	}
	if err := s.store.UpsertInstance(ctx, row); err != nil {
		return nil, err
	}
	saved, err := s.store.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, fmt.Errorf("保存后未找到实例")
	}
	v := instanceViewFromRow(*saved)
	return &v, nil
}

func (s *FlashbackImpl) DeleteInstance(ctx context.Context, id string) error {
	return s.store.DeleteInstance(ctx, id)
}

var flashbackCloudVendorNames = []struct {
	code, name string
}{
	{flashbackVendorTencent, "腾讯云"},
	{flashbackVendorAliyun, "阿里云"},
	{flashbackVendorHuawei, "华为云"},
	{flashbackVendorAWS, "AWS"},
}

var flashbackEditableArgs = []struct {
	Key, Description string
	Secret           bool
}{
	{gaFlashbackTencentSecretID, "腾讯云 SecretId", true},
	{gaFlashbackTencentSecretKey, "腾讯云 SecretKey", true},
	{gaFlashbackTencentRegion, "腾讯云默认 Region，如 ap-guangzhou", false},
	{gaFlashbackTencentRegionMap, "腾讯云 Region 映射 JSON（可选）", false},
	{gaFlashbackAliyunAccessKeyID, "阿里云 AccessKey ID", true},
	{gaFlashbackAliyunAccessKeySecret, "阿里云 AccessKey Secret", true},
	{gaFlashbackHuaweiAccessKeyID, "华为云 AccessKey ID", true},
	{gaFlashbackHuaweiSecretAccessKey, "华为云 Secret Access Key", true},
	{gaFlashbackAWSAccessKeyID, "AWS AccessKey ID", true},
	{gaFlashbackAWSSecretAccessKey, "AWS Secret Access Key", true},
	{gaFlashbackCloudLookbackHours, "云日志回溯小时数", false},
	{gaFlashbackCloudDownloadMbps, "云日志下载限速 Mbps", false},
	{gaFlashbackCloudAPIIntervalMS, "云 API 间隔毫秒", false},
	{gaFlashbackCloudMaxPackages, "单次最多增量包数", false},
	{gaFlashbackCloudPkgRetries, "增量包下载重试次数", false},
}

func flashbackAllowedArgKey(key string) bool {
	key = strings.TrimSpace(key)
	for _, it := range flashbackEditableArgs {
		if it.Key == key {
			return true
		}
	}
	return false
}

// CloudSettings 返回可编辑的多云参数（含当前生效值）。key 与 Hub global_args 一致。
func (s *FlashbackImpl) CloudSettings(ctx context.Context) dto.FlashbackCloudSettings {
	out := dto.FlashbackCloudSettings{
		TencentRegion:     flashbackTencentRegion(ctx),
		Vendors:           make([]dto.FlashbackCloudVendorStatus, 0, len(flashbackCloudVendorNames)),
		Args:              make([]dto.FlashbackArgItem, 0, len(flashbackEditableArgs)),
		OfflineAllowPaths: flashbackPDUAllowPaths(ctx),
		OfflineRoot:       flashbackPDUOfflineRoot(ctx),
	}
	for _, v := range flashbackCloudVendorNames {
		idKey, keyKey, _, _, err := flashbackCloudVendorKeyPair(v.code)
		if err != nil {
			continue
		}
		_, _, credErr := flashbackCloudVendorCreds(ctx, v.code)
		out.Vendors = append(out.Vendors, dto.FlashbackCloudVendorStatus{
			Vendor: v.code, Name: v.name, IDKey: idKey, KeyKey: keyKey, Configured: credErr == nil,
		})
	}
	for _, meta := range flashbackEditableArgs {
		val, src := lookupFlashbackArgSource(ctx, meta.Key)
		out.Args = append(out.Args, dto.FlashbackArgItem{
			Key: meta.Key, Value: val, Description: meta.Description, Secret: meta.Secret, Source: src,
		})
	}
	return out
}

// SaveCloudSettings 把多云参数写入 tbl_flashback_args，立即生效。
func (s *FlashbackImpl) SaveCloudSettings(ctx context.Context, req *dto.FlashbackCloudSettingsSave) (*dto.FlashbackCloudSettings, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为空")
	}
	for _, item := range req.Args {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			continue
		}
		if !flashbackAllowedArgKey(key) {
			return nil, fmt.Errorf("不支持的参数 key: %s", key)
		}
		desc := strings.TrimSpace(item.Description)
		if desc == "" {
			for _, meta := range flashbackEditableArgs {
				if meta.Key == key {
					desc = meta.Description
					break
				}
			}
		}
		if err := s.store.UpsertArg(ctx, key, item.Value, desc); err != nil {
			return nil, err
		}
	}
	out := s.CloudSettings(ctx)
	return &out, nil
}

// FlashbackBootstrap 检查人工建表是否就绪，并回收异常中断的 running 任务。不自动建表。
func FlashbackBootstrap(ctx context.Context) error {
	flashbackBootstrapOnce.Do(func() {
		defer func() {
			if rec := recover(); rec != nil {
				flashbackBootstrapErr = fmt.Errorf("flashback bootstrap: %v", rec)
			}
		}()
		if err := flashbackStore.EnsureSchema(ctx); err != nil {
			flashbackBootstrapErr = err
			return
		}
		n, err := flashbackStore.FailStuckRunning(ctx, "进程退出后任务未跑完（重启、部署或资源不足），无法续跑")
		if err != nil {
			log.Warn("flashback fail stuck running", zap.Error(err))
		} else if n > 0 {
			log.Info("flashback reclaimed stuck tasks", zap.Int64("count", n))
		}
		cleaned, err := flashbackCleanupOrphanWorkDirs(flashbackWorkDirBase(ctx))
		if err != nil {
			log.Warn("flashback cleanup orphan wal", zap.Error(err))
		} else if cleaned > 0 {
			log.Info("flashback cleaned orphan wal dirs", zap.Int("count", cleaned))
		}
	})
	return flashbackBootstrapErr
}

func (s *FlashbackImpl) ensureReady(ctx context.Context) error {
	if err := FlashbackBootstrap(ctx); err != nil {
		return err
	}
	return nil
}

func (s *FlashbackImpl) Create(c *gin.Context, req *dto.FlashbackTaskReq) (*dto.FlashbackTask, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	pre, err := s.Precheck(c, req)
	if err != nil {
		return nil, err
	}
	if pre == nil || !pre.OK {
		msg := "预检查未通过"
		if pre != nil {
			for _, it := range pre.Items {
				if it.Status == flashbackCheckFailed {
					msg = it.Name + ": " + it.Message
					break
				}
			}
		}
		return nil, fmt.Errorf("%s", msg)
	}
	ctx := c.Request.Context()
	target, _ := flashbackParseTime(req.TargetTime)
	var endPtr *time.Time
	if strings.TrimSpace(req.EndTime) != "" {
		et, _ := flashbackParseTime(req.EndTime)
		endPtr = &et
	}
	kind := strings.ToLower(strings.TrimSpace(req.OutputKind))
	if kind == "" {
		kind = flashback.OutputFlashback
	}
	createdBy := resolveCdcOperator(c)
	row := &flashback.TaskRow{
		ID:           flashback.NewID(),
		Host:         pre.Host,
		Port:         pre.Port,
		DatabaseName: strings.TrimSpace(req.Database),
		Tables:       flashbackTablesJSON(req.Tables),
		TargetTime:   target,
		EndTime:      endPtr,
		StartXID:     req.StartXID,
		StopXID:      req.StopXID,
		StartFile:    strings.TrimSpace(req.StartFile),
		StartPos:     req.StartPos,
		StopFile:     strings.TrimSpace(req.StopFile),
		StopPos:      req.StopPos,
		SQLType:      strings.TrimSpace(req.SQLType),
		OutputKind:   kind,
		Engine:       flashback.EngineNative,
		Status:       flashback.StatusPending,
		CreatedBy:    createdBy,
	}
	if flashbackEngineIsPDU(req) {
		row.Engine = flashback.EnginePDU
		row.Extra = flashbackMarshalPDUExtra(flashbackPDUExtraFromReq(req))
		if strings.TrimSpace(req.InstanceID) == "" {
			row.InstanceID = flashbackPDULocalInstance
		} else {
			row.InstanceID = strings.TrimSpace(req.InstanceID)
		}
		if row.Host == "" {
			row.Host = "local"
		}
	} else if err := flashbackAssignTaskHubDomain(ctx, row, req.InstanceID, pre.Host, pre.Port); err != nil {
		return nil, err
	}
	if err := s.store.InsertTask(ctx, row); err != nil {
		return nil, err
	}
	if saved, err := s.store.GetTask(ctx, row.ID); err == nil && saved != nil {
		row = saved
	}
	_ = s.store.InsertLog(ctx, row.ID, "INFO", "任务已创建，排队等待执行（全局同时只跑 1 个闪回任务）")
	if !flashbackEngineIsPDU(req) {
		s.persistTaskDictionary(ctx, row.ID, req)
	}
	go s.runTask(context.Background(), row.ID)
	return s.taskDTO(ctx, row), nil
}

func (s *FlashbackImpl) persistTaskDictionary(ctx context.Context, taskID string, req *dto.FlashbackTaskReq) {
	if req == nil {
		return
	}
	if src := strings.TrimSpace(req.DictTaskID); src != "" {
		if err := flashbackCopyDictionaryFile(ctx, src, taskID); err != nil {
			_ = s.store.InsertLog(ctx, taskID, "WARN", "复用源任务字典失败，将在解析时现场导出: "+err.Error())
			return
		}
		_ = s.store.InsertLog(ctx, taskID, "INFO", "已从任务 "+src+" 加载数据字典快照")
		return
	}
	db, dom, _, err := flashbackConnectTarget(ctx, req.InstanceID, req.Database)
	if err != nil {
		_ = s.store.InsertLog(ctx, taskID, "WARN", "创建时导出字典失败，将在解析时重试: "+err.Error())
		return
	}
	defer db.Close()
	if dom != nil && flashbackIsMySQL(dom.DbType) {
		dict, err := flashbackLoadMySQLDictionary(ctx, db, req.Database, req.Tables)
		if err != nil {
			_ = s.store.InsertLog(ctx, taskID, "WARN", "创建时导出 MySQL 字典失败: "+err.Error())
			return
		}
		if err := flashbackSaveMySQLDictionaryFile(flashbackDictPath(ctx, taskID), dict); err != nil {
			_ = s.store.InsertLog(ctx, taskID, "WARN", "保存 MySQL 字典快照失败: "+err.Error())
			return
		}
		_ = s.store.InsertLog(ctx, taskID, "INFO", fmt.Sprintf("已保存 MySQL 数据字典快照（%d 张表）", len(dict.Wanted)))
		return
	}
	dict, err := flashbackLoadDictionary(ctx, db, req.Database, req.Tables)
	if err != nil {
		_ = s.store.InsertLog(ctx, taskID, "WARN", "创建时导出字典失败: "+err.Error())
		return
	}
	if err := flashbackSaveDictionaryFile(flashbackDictPath(ctx, taskID), dict); err != nil {
		_ = s.store.InsertLog(ctx, taskID, "WARN", "保存字典快照失败: "+err.Error())
		return
	}
	_ = s.store.InsertLog(ctx, taskID, "INFO", fmt.Sprintf("已保存数据字典快照（%d 张表）", len(dict.Wanted)))
}

func (s *FlashbackImpl) Get(c *gin.Context, id string) (*dto.FlashbackTask, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	row, err := s.store.GetTask(c.Request.Context(), strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("task not found")
	}
	return s.taskDTO(c.Request.Context(), row), nil
}

func (s *FlashbackImpl) List(c *gin.Context) (*dto.FlashbackTaskList, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "10"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 10
	}
	if size > 200 {
		size = 200
	}
	offset := (page - 1) * size
	rows, total, err := s.store.ListTasks(c.Request.Context(), flashback.TaskListFilter{
		InstanceID: c.Query("instance_id"),
		Status:     c.Query("status"),
		Keyword:    c.Query("keyword"),
		Offset:     offset,
		Limit:      size,
	})
	if err != nil {
		return nil, err
	}
	out := &dto.FlashbackTaskList{Total: total}
	ctx := c.Request.Context()
	for _, r := range rows {
		out.List = append(out.List, s.taskDTO(ctx, r))
	}
	if out.List == nil {
		out.List = []*dto.FlashbackTask{}
	}
	return out, nil
}

func (s *FlashbackImpl) ListSQL(c *gin.Context, id string) (*dto.FlashbackSQLList, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	ctx := c.Request.Context()
	row, err := s.store.GetTask(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("task not found")
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if page < 1 {
		page = 1
	}
	kind, ops := flashbackSQLPreviewFilter(row.OutputKind, row.SQLType, c.Query("kind"), c.Query("sql_type"))
	rows, total, err := s.store.ListSQLs(ctx, row.ID, kind, ops, (page-1)*size, size)
	if err != nil {
		return nil, err
	}
	out := &dto.FlashbackSQLList{Total: total}
	for _, r := range rows {
		out.List = append(out.List, &dto.FlashbackSQLItem{
			Seq: r.Seq, Kind: r.Kind, SchemaName: r.SchemaName, TableName: r.TableName,
			Op: r.Op, XID: r.XID, TS: r.TS, Statement: r.Statement, Risk: r.Risk,
		})
	}
	if out.List == nil {
		out.List = []*dto.FlashbackSQLItem{}
	}
	return out, nil
}

func (s *FlashbackImpl) ListLogs(c *gin.Context, id string) ([]dto.FlashbackLogItem, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	rows, err := s.store.ListLogs(c.Request.Context(), strings.TrimSpace(id), 200)
	if err != nil {
		return nil, err
	}
	out := make([]dto.FlashbackLogItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, dto.FlashbackLogItem{Level: r.Level, Message: r.Message, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

func (s *FlashbackImpl) ListArtifacts(c *gin.Context, id string) ([]dto.FlashbackArtifact, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return nil, err
	}
	rows, err := s.store.ListArtifacts(c.Request.Context(), strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	out := make([]dto.FlashbackArtifact, 0, len(rows))
	for _, r := range rows {
		out = append(out, dto.FlashbackArtifact{Kind: r.Kind, Name: r.RelPath, Bytes: r.Bytes, RowCount: r.RowCount})
	}
	return out, nil
}

func (s *FlashbackImpl) ArtifactFile(c *gin.Context, id, name string) (string, error) {
	if err := s.ensureReady(c.Request.Context()); err != nil {
		return "", err
	}
	work := filepath.Join(flashbackWorkDirBase(c.Request.Context()), strings.TrimSpace(id))
	full, err := flashbackPDUSafeJoin(work, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(full); err != nil {
		return "", fmt.Errorf("产物不存在")
	}
	return full, nil
}

func (s *FlashbackImpl) SubmitDMLTicket(_ *gin.Context, _ string, _ *dto.FlashbackSubmitDMLReq) (map[string]any, error) {
	return nil, fmt.Errorf("独立闪回服务不提交工单")
}

func flashbackTaskFromRow(r *flashback.TaskRow) *dto.FlashbackTask {
	if r == nil {
		return nil
	}
	var tables []string
	_ = json.Unmarshal([]byte(r.Tables), &tables)
	if tables == nil {
		tables = []string{}
	}
	hubID := flashbackSanitizeTaskIDs(r.ID, r.InstanceID)
	mdmID := flashbackSanitizeTaskIDs(r.ID, r.MDMInstanceID)
	t := &dto.FlashbackTask{
		ID: r.ID, InstanceID: hubID, DomainInstanceID: hubID, MDMInstanceID: mdmID,
		Host: r.Host, Port: r.Port, Database: r.DatabaseName, Tables: tables,
		TargetTime: r.TargetTime, EndTime: r.EndTime, StartXID: r.StartXID, StopXID: r.StopXID,
		StartFile: r.StartFile, StartPos: r.StartPos, StopFile: r.StopFile, StopPos: r.StopPos,
		SQLType: r.SQLType, OutputKind: r.OutputKind, Status: r.Status,
		ErrorMessage: flashbackPublicErrorMessage(r.ErrorMessage), Warning: r.Warning, WorkDir: r.WorkDir,
		WALBytes: r.WALBytes, WALFiles: r.WALFiles, ChangeCount: r.ChangeCount,
		Progress:    flashbackProgressFromRow(r),
		DMLTicketID: r.DMLTicketID, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
	flashbackApplyPDUExtraToTask(t, r)
	return t
}

func flashbackStageRemain(done, total int) int {
	if total <= 0 {
		return 0
	}
	if done <= 0 {
		return total
	}
	if done >= total {
		return 0
	}
	return total - done
}

func flashbackProgressUnit(logTotal, parseTotal int) string {
	if logTotal >= 256 || parseTotal >= 256 {
		return "bytes"
	}
	return "files"
}

func flashbackStagePercent(done, total int) int {
	if total <= 0 {
		return 0
	}
	if done <= 0 {
		return 0
	}
	if done >= total {
		return 100
	}
	return int(int64(done) * 100 / int64(total))
}

func flashbackProgressPhase(status string, logDone, logTotal, parseDone, parseTotal int) string {
	switch status {
	case flashback.StatusSucceeded:
		return "done"
	case flashback.StatusFailed, flashback.StatusCancelled:
		return "failed"
	case flashback.StatusPending:
		return "pending"
	}
	if logTotal <= 0 {
		return "fetch_logs"
	}
	if logDone < logTotal && logDone <= parseDone {
		return "fetch_logs"
	}
	if parseTotal <= 0 || parseDone < parseTotal {
		return "parse"
	}
	return "parse"
}

func flashbackProgressFromRow(r *flashback.TaskRow) *dto.FlashbackTaskProgress {
	if r == nil {
		return nil
	}
	p := &dto.FlashbackTaskProgress{
		Unit:        flashbackProgressUnit(r.LogTotal, r.ParseTotal),
		FetchDone:   r.LogDone,
		FetchTotal:  r.LogTotal,
		FetchRemain: flashbackStageRemain(r.LogDone, r.LogTotal),
		ParseDone:   r.ParseDone,
		ParseTotal:  r.ParseTotal,
		ParseRemain: flashbackStageRemain(r.ParseDone, r.ParseTotal),
	}
	p.FetchPercent = flashbackStagePercent(r.LogDone, r.LogTotal)
	p.ParsePercent = flashbackStagePercent(r.ParseDone, r.ParseTotal)
	p.Phase = flashbackProgressPhase(r.Status, r.LogDone, r.LogTotal, r.ParseDone, r.ParseTotal)
	if p.Phase == "done" {
		if r.LogTotal > 0 {
			p.FetchDone = r.LogTotal
			p.FetchPercent = 100
		} else {
			p.FetchPercent = 100
		}
		if r.ParseTotal > 0 {
			p.ParseDone = r.ParseTotal
			p.ParsePercent = 100
		} else {
			p.ParsePercent = 100
		}
		p.FetchRemain = 0
		p.ParseRemain = 0
	}
	return p
}

// flashbackPublicError 把 Hub 元库驱动错误转成任务页文案。
// 元库是 PostgreSQL，lib/pq 会带 "pq:" 前缀；MySQL 任务不能原样露出，否则看起来像目标库报错。
func flashbackPublicError(err error) string {
	if err == nil {
		return ""
	}
	return flashbackPublicErrorMessage(err.Error())
}

func flashbackPublicErrorMessage(msg string) string {
	raw := strings.TrimSpace(msg)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "写入平台任务记录失败：") {
		return raw
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "invalid byte sequence for encoding") {
		return "写入平台任务记录失败：生成的闪回 SQL 含非法 UTF-8 字节，不是目标实例报错"
	}
	if flashbackMySQLDumpTransientMsg(lower) {
		return flashbackMySQLDumpBrokenHint
	}
	if i := strings.Index(raw, "pq:"); i >= 0 {
		detail := strings.TrimSpace(raw[i+3:])
		if detail == "" {
			return "写入平台任务记录失败"
		}
		return "写入平台任务记录失败：" + detail
	}
	return raw
}
