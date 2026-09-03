package service

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"db-flashback/internal/storage/flashback"
)

const (
	flashbackMySQLDumpHeartbeat   = 15 * time.Second
	flashbackMySQLDumpReadTimeout = 90 * time.Second
	flashbackMySQLDumpReconnect   = 8
	flashbackMySQLDumpOuterRetry  = 4
	flashbackMySQLMaxPendingRows  = 10000
	flashbackMySQLDumpBrokenHint  = "BINLOG DUMP 连接中断：实例或中间网络断开。常见原因：时间窗过大扫了整段 binlog、并发 DUMP 过多、或 Hub 内存不足被系统杀掉。请缩小时间窗后重试"
)

func flashbackMySQLDumpSyncerConfig(creds flashbackMySQLCreds, serverID uint32) replication.BinlogSyncerConfig {
	return replication.BinlogSyncerConfig{
		ServerID:             serverID,
		Flavor:               "mysql",
		Host:                 creds.Host,
		Port:                 uint16(creds.Port),
		User:                 creds.User,
		Password:             creds.Password,
		ParseTime:            true,
		UseDecimal:           true,
		DisableRetrySync:     false,
		MaxReconnectAttempts: flashbackMySQLDumpReconnect,
		HeartbeatPeriod:      flashbackMySQLDumpHeartbeat,
		ReadTimeout:          flashbackMySQLDumpReadTimeout,
		RecvBufferSize:       1 << 20,
	}
}

func flashbackMySQLDumpTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return flashbackMySQLDumpTransientMsg(err.Error())
}

func flashbackMySQLDumpTransientMsg(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "unexpected eof") ||
		strings.Contains(s, "connection was bad") ||
		strings.Contains(s, "io.copyn") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "use of closed network")
}

func flashbackMySQLDumpPublicErr(err error) error {
	if err == nil {
		return nil
	}
	if flashbackMySQLDumpTransient(err) {
		return fmt.Errorf("%s", flashbackMySQLDumpBrokenHint)
	}
	return err
}

type flashbackMySQLDumpStat struct {
	Events      int
	Rows        int
	DDLSkipped  int
	Bytes       int64
	ByteTrunc   bool
	ChangeTrunc bool
	StartFile   string
	StopFile    string
}

func (s flashbackMySQLDumpStat) String() string {
	return fmt.Sprintf("events=%d rows=%d ddl_skipped=%d bytes=%d start=%s stop=%s",
		s.Events, s.Rows, s.DDLSkipped, s.Bytes, s.StartFile, s.StopFile)
}

func flashbackMySQLIsDDLQuery(q string) bool {
	return flashbackMySQLDDLOp(q) != ""
}

func flashbackMySQLDDLOp(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	if flashbackMySQLTxnBoundary(q) != "" {
		return ""
	}
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	switch strings.ToUpper(fields[0]) {
	case "CREATE":
		return "CREATE"
	case "ALTER":
		return "ALTER"
	case "DROP":
		return "DROP"
	case "TRUNCATE":
		return "TRUNCATE"
	case "RENAME":
		return "ALTER"
	}
	return ""
}

func flashbackMySQLTxnBoundary(q string) string {
	u := strings.ToUpper(strings.TrimSpace(q))
	switch u {
	case "BEGIN", "COMMIT", "ROLLBACK":
		return strings.ToLower(u)
	}
	if strings.HasPrefix(u, "BEGIN") || strings.HasPrefix(u, "START TRANSACTION") {
		return "begin"
	}
	if strings.HasPrefix(u, "ROLLBACK") {
		return "rollback"
	}
	if strings.HasPrefix(u, "COMMIT") {
		return "commit"
	}
	return ""
}

func flashbackMySQLLiteral(v any, dataType string) string {
	if v == nil {
		return "NULL"
	}
	dt := strings.ToLower(strings.TrimSpace(dataType))
	switch val := v.(type) {
	case bool:
		if val {
			return `\RAW:1`
		}
		return `\RAW:0`
	case int:
		return `\RAW:` + strconv.Itoa(val)
	case int8:
		return `\RAW:` + strconv.FormatInt(int64(val), 10)
	case int16:
		return `\RAW:` + strconv.FormatInt(int64(val), 10)
	case int32:
		return `\RAW:` + strconv.FormatInt(int64(val), 10)
	case int64:
		if dt == "bit" {
			return `\RAW:` + strconv.FormatInt(val, 10)
		}
		if dt == "enum" || dt == "set" {
			return strconv.FormatInt(val, 10)
		}
		return `\RAW:` + strconv.FormatInt(val, 10)
	case uint:
		return `\RAW:` + strconv.FormatUint(uint64(val), 10)
	case uint8:
		return `\RAW:` + strconv.FormatUint(uint64(val), 10)
	case uint16:
		return `\RAW:` + strconv.FormatUint(uint64(val), 10)
	case uint32:
		return `\RAW:` + strconv.FormatUint(uint64(val), 10)
	case uint64:
		return `\RAW:` + strconv.FormatUint(val, 10)
	case float32:
		return `\RAW:` + strconv.FormatFloat(float64(val), 'f', -1, 32)
	case float64:
		return `\RAW:` + strconv.FormatFloat(val, 'f', -1, 64)
	case []byte:
		if (dt == "json" || strings.Contains(dt, "json") ||
			dt == "text" || dt == "tinytext" || dt == "mediumtext" || dt == "longtext" ||
			dt == "char" || dt == "varchar" || dt == "enum" || dt == "set") && utf8.Valid(val) {
			return string(val)
		}
		return `\RAW:_binary 0x` + hex.EncodeToString(val)
	case time.Time:
		switch dt {
		case "date":
			return val.Format("2006-01-02")
		case "time":
			return val.Format("15:04:05.999999")
		case "year":
			return `\RAW:` + val.Format("2006")
		default:
			return val.Format("2006-01-02 15:04:05.999999")
		}
	case string:
		if dt == "decimal" || dt == "numeric" || dt == "float" || dt == "double" || dt == "newdecimal" {
			return `\RAW:` + val
		}
		return val
	default:
		if s, ok := v.(interface{ String() string }); ok {
			if dt == "decimal" || dt == "numeric" || dt == "newdecimal" {
				return `\RAW:` + s.String()
			}
			return s.String()
		}
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
}

func flashbackMySQLRowMap(rel *flashbackMySQLRel, row []any, tm *replication.TableMapEvent) map[string]string {
	out := map[string]string{}
	n := len(row)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("col_%d", i)
		typ := ""
		if rel != nil && i < len(rel.Columns) {
			name = rel.Columns[i].Name
			typ = rel.Columns[i].DataType
		} else if tm != nil && i < len(tm.ColumnName) && len(tm.ColumnName[i]) > 0 {
			name = string(tm.ColumnName[i])
		}
		if tm != nil && (typ == "enum" || typ == "set") {
			if s := flashbackMySQLEnumOrSet(tm, i, row[i], typ); s != "" {
				out[name] = s
				continue
			}
		}
		out[name] = flashbackMySQLLiteral(row[i], typ)
	}
	return out
}

func flashbackMySQLEnumOrSet(tm *replication.TableMapEvent, idx int, v any, typ string) string {
	if tm == nil {
		return ""
	}
	n, ok := v.(int64)
	if !ok {
		return ""
	}
	if typ == "enum" {
		labels := tm.EnumStrValueMap()[idx]
		if n > 0 && int(n-1) < len(labels) {
			return labels[n-1]
		}
	}
	if typ == "set" {
		vals := tm.SetStrValueMap()[idx]
		var parts []string
		for i := 0; i < len(vals) && i < 64; i++ {
			if n&(1<<uint(i)) != 0 {
				parts = append(parts, vals[i])
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ",")
		}
	}
	return ""
}

func flashbackMySQLEventOp(ev *replication.BinlogEvent) string {
	if ev == nil {
		return ""
	}
	switch ev.Header.EventType {
	case replication.WRITE_ROWS_EVENTv0, replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		return "INSERT"
	case replication.UPDATE_ROWS_EVENTv0, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2,
		replication.PARTIAL_UPDATE_ROWS_EVENT:
		return "UPDATE"
	case replication.DELETE_ROWS_EVENTv0, replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		return "DELETE"
	default:
		return ""
	}
}

func (s *FlashbackImpl) executeTaskMySQL(ctx context.Context, taskID string, row *flashback.TaskRow, db *sql.DB) error {
	s.logf(ctx, taskID, "INFO", "在线模式：BINLOG DUMP，binlog 不下载到 Hub")
	if err := flashbackMySQLRejectOfflineWorkDir(row.WorkDir); err != nil {
		return err
	}
	var tables []string
	_ = json.Unmarshal([]byte(row.Tables), &tables)
	dict, src, err := flashbackOpenTaskMySQLDictionary(ctx, db, taskID, row.DatabaseName, tables)
	if err != nil {
		return err
	}
	s.logf(ctx, taskID, "INFO", "已加载 MySQL 数据字典（%s，%d 张表）", src, len(dict.Wanted))
	for _, rel := range dict.Wanted {
		if rel == nil {
			continue
		}
		if rel.NoPK {
			s.logf(ctx, taskID, "WARN", "表 %s.%s 无主键，回滚 SQL 使用全列等值", rel.Schema, rel.Name)
		}
	}

	creds, _, err := flashbackResolveMySQLCreds(ctx, row.InstanceID, row.DatabaseName, row.Host, row.Port)
	if err != nil {
		return err
	}
	end := time.Now()
	if row.EndTime != nil {
		end = *row.EndTime
	}
	if !end.After(row.TargetTime) {
		return fmt.Errorf("end_time 必须晚于 target_time")
	}
	logs, err := flashbackMySQLListBinlogs(ctx, db)
	if err != nil {
		return fmt.Errorf("SHOW BINARY LOGS: %w", err)
	}
	if len(logs) == 0 {
		return fmt.Errorf("没有可 DUMP 的 binlog 文件")
	}
	pickedStart := ""
	if strings.TrimSpace(row.StartFile) == "" {
		startIdx := flashbackMySQLPickStartFile(ctx, creds, logs, row.TargetTime)
		if startIdx < 0 || startIdx >= len(logs) {
			startIdx = 0
		}
		pickedStart = logs[startIdx].Name
	}
	masterFile, masterPos, err := flashbackMySQLMasterStatus(ctx, db)
	if err != nil {
		s.logf(ctx, taskID, "WARN", "读取 MASTER STATUS 失败，将按 end_time / stop_file 停：%v", err)
	}
	startFile, startPos, endFile, endPos, err := flashbackMySQLResolveDumpRange(
		row.StartFile, row.StartPos, row.StopFile, row.StopPos,
		logs, pickedStart, masterFile, masterPos)
	if err != nil {
		return err
	}

	var avoid uint32
	if sid := flashbackMySQLVar(ctx, db, "server_id"); sid != "" {
		n, _ := strconv.ParseUint(sid, 10, 32)
		avoid = uint32(n)
	}
	dumpID := flashbackMySQLDumpServerID(taskID, avoid)
	s.logf(ctx, taskID, "INFO", "时间窗 %s ~ %s，从 %s:%d 开始 BINLOG DUMP（server_id=%d，停于 %s:%d 或 end_time；xid=%d~%d；窗前事件跳过）",
		row.TargetTime.Format(time.RFC3339), end.Format(time.RFC3339), startFile, startPos, dumpID, endFile, endPos, row.StartXID, row.StopXID)

	dumpTotal := flashbackMySQLDumpRemainBytes(logs, startFile, startPos, endFile, endPos)
	_, tot0 := flashbackMySQLByteProgress(0, dumpTotal, false)
	s.writeFlashbackProgress(ctx, taskID, "", 0, 0, 0, 0, tot0, 0, tot0)
	if dumpTotal > 0 {
		s.logf(ctx, taskID, "INFO", "预估待扫 binlog %s（%s ~ %s，剩余 %s）",
			flashbackFormatBytes(dumpTotal), startFile, endFile, flashbackFormatBytes(dumpTotal))
	}

	maxBytes := flashbackMaxWALBytes(ctx)
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
		xid := flashbackChangeXID(ch)
		if row.StartXID > 0 && (xid == 0 || xid < row.StartXID) {
			return true
		}
		if row.StopXID > 0 && xid > row.StopXID {
			return true
		}
		if !flashbackWantOp(opFilter, ch.Op) {
			return true
		}
		if !flashbackIsDDLOp(ch.Op) && dict.match(ch.Schema, ch.Table) == nil {
			return true
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
		wrote := false
		if undo != "" {
			batch = append(batch, &flashback.SQLRow{
				TaskID: taskID, Seq: seq, Kind: flashback.KindUndo,
				SchemaName: ch.Schema, TableName: ch.Table, Op: ch.Op,
				XID: xid, TS: ts, Statement: undo, Risk: ur,
			})
			seq++
			wrote = true
		}
		if redo != "" {
			batch = append(batch, &flashback.SQLRow{
				TaskID: taskID, Seq: seq, Kind: flashback.KindRedo,
				SchemaName: ch.Schema, TableName: ch.Table, Op: ch.Op,
				XID: xid, TS: ts, Statement: redo, Risk: rr,
			})
			seq++
			wrote = true
		}
		if wrote {
			changeCount++
		} else {
			skipped++
		}
		if len(batch) >= flashbackSQLInsertBatch {
			if err := flush(); err != nil {
				flushErr = err
				return false
			}
		}
		return changeCount < maxSQLs
	}

	st, err := flashbackDumpMySQLBinlog(ctx, creds, dumpID, flashbackMySQLDumpOpt{
		StartFile: startFile, StartPos: startPos,
		EndFile: endFile, EndPos: endPos,
		Target: row.TargetTime, End: end,
		StartXID: row.StartXID, StopXID: row.StopXID,
		MaxBytes: maxBytes,
	}, dict, func(file string, n int64, logPos uint32) {
		read := flashbackMySQLDumpReadBytes(logs, startFile, startPos, file, logPos)
		if n > read {
			read = n
		}
		done, tot := flashbackMySQLByteProgress(read, dumpTotal, false)
		remain := dumpTotal - read
		if remain < 0 {
			remain = 0
		}
		if dumpTotal > 0 {
			s.logf(ctx, taskID, "INFO", "正在解析 binlog %s（已读 %s / 共 %s，剩余 %s）",
				file, flashbackFormatBytes(read), flashbackFormatBytes(dumpTotal), flashbackFormatBytes(remain))
		} else {
			s.logf(ctx, taskID, "INFO", "正在解析 binlog %s（已读 %s）", file, flashbackFormatBytes(read))
		}
		s.writeFlashbackProgress(ctx, taskID, "", read, done, changeCount, done, tot, done, tot)
	}, handle)
	if err != nil {
		return fmt.Errorf("DUMP/解析 binlog: %w", err)
	}
	if flushErr != nil {
		return flushErr
	}
	if err := flush(); err != nil {
		return err
	}
	s.logf(ctx, taskID, "INFO", "binlog 解析统计：%s", st.String())
	doneEnd, totEnd := flashbackMySQLByteProgress(dumpTotal, dumpTotal, true)
	if dumpTotal <= 0 {
		doneEnd, totEnd = 1, 1
	}
	s.writeFlashbackProgress(ctx, taskID, "", st.Bytes, 1, changeCount, doneEnd, totEnd, doneEnd, totEnd)

	warn := ""
	if st.ByteTrunc {
		warn = "binlog 体积超过上限，已截断。"
	}
	if sqlTrunc || st.ChangeTrunc {
		warn += fmt.Sprintf("undo SQL 超过上限 %d，已截断。请缩小时间窗或拆表。", maxSQLs)
	}
	if st.DDLSkipped > 0 {
		warn += fmt.Sprintf("时间窗内看到 %d 条 DDL/TRUNCATE，已落原文审计，不生成反向 SQL。", st.DDLSkipped)
	}
	if changeCount == 0 {
		warn += "未能从 binlog 解析出行级变更。" + st.String() + "。常见原因：时间窗内无该表 DML，或 binlog 已 purge。"
		s.logf(ctx, taskID, "WARN", "%s", warn)
	} else {
		s.logf(ctx, taskID, "INFO", "生成 undo %d 条（跳过 %d）", changeCount, skipped)
	}
	return s.store.UpdateStatus(ctx, taskID, flashback.StatusSucceeded, "", strings.TrimSpace(warn))
}

const flashbackMySQLDumpProbeTimeout = 8 * time.Second

func flashbackMySQLDumpProbeMessage(err error) string {
	if err == nil {
		return "BINLOG DUMP 可用"
	}
	return fmt.Sprintf("BINLOG DUMP 失败：%v。厂商可能未开放 BINLOG DUMP / 账号缺复制权限 / 需在控制台开启本地 binlog", err)
}

const flashbackMySQLMaxFileProbes = 32
const flashbackMySQLFirstEventTimeout = 5 * time.Second

func flashbackMySQLFirstEventTime(ctx context.Context, creds flashbackMySQLCreds, file string) (time.Time, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		return time.Time{}, fmt.Errorf("empty binlog file")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, flashbackMySQLFirstEventTimeout)
	defer cancel()
	cfg := flashbackMySQLDumpSyncerConfig(creds, flashbackMySQLDumpServerID("first-ts:"+file))
	syncer := replication.NewBinlogSyncer(cfg)
	defer syncer.Close()
	streamer, err := syncer.StartSync(gomysql.Position{Name: file, Pos: 4})
	if err != nil {
		return time.Time{}, err
	}
	for i := 0; i < 8; i++ {
		ev, err := streamer.GetEvent(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if ev == nil || ev.Header == nil || ev.Header.Timestamp == 0 {
			continue
		}
		return time.Unix(int64(ev.Header.Timestamp), 0), nil
	}
	return time.Time{}, fmt.Errorf("binlog %s 前几个事件没有时间戳", file)
}

func flashbackMySQLRangeFileCount(logs []flashbackMySQLBinlogFile, startFile, endFile string) int {
	n := 0
	started := strings.TrimSpace(startFile) == ""
	for _, l := range logs {
		if !started {
			if l.Name == startFile {
				started = true
			} else {
				continue
			}
		}
		n++
		if strings.TrimSpace(endFile) != "" && l.Name == endFile {
			break
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

func flashbackMySQLRangeFileIndex(logs []flashbackMySQLBinlogFile, startFile, current string) int {
	started := strings.TrimSpace(startFile) == ""
	idx := 0
	for _, l := range logs {
		if !started {
			if l.Name == startFile {
				started = true
			} else {
				continue
			}
		}
		idx++
		if l.Name == current {
			return idx
		}
	}
	if idx < 1 {
		return 1
	}
	return idx
}

func flashbackMySQLDumpRemainBytes(logs []flashbackMySQLBinlogFile, startFile string, startPos uint32, endFile string, endPos uint32) int64 {
	startFile = strings.TrimSpace(startFile)
	endFile = strings.TrimSpace(endFile)
	if len(logs) == 0 {
		return 0
	}
	var total int64
	started := startFile == ""
	for _, l := range logs {
		if !started {
			if l.Name != startFile {
				continue
			}
			started = true
		}
		from := int64(0)
		to := l.Size
		if l.Name == startFile && startPos > 0 {
			from = int64(startPos)
		}
		if endFile != "" && l.Name == endFile && endPos > 0 {
			to = int64(endPos)
		}
		if to > from {
			total += to - from
		}
		if endFile != "" && l.Name == endFile {
			break
		}
	}
	return total
}

func flashbackMySQLDumpReadBytes(logs []flashbackMySQLBinlogFile, startFile string, startPos uint32, curFile string, logPos uint32) int64 {
	curFile = strings.TrimSpace(curFile)
	if curFile == "" || logPos == 0 {
		return 0
	}
	return flashbackMySQLDumpRemainBytes(logs, startFile, startPos, curFile, logPos)
}

func flashbackMySQLByteProgress(read, total int64, finished bool) (done, tot int) {
	done, tot = flashbackScaleByteProgress(read, total)
	if finished {
		if tot < 1 {
			return 1, 1
		}
		return tot, tot
	}
	if tot > 0 && done >= tot {
		done = tot - 1
	}
	return done, tot
}

const flashbackProgressIntMax = 2000000000

func flashbackScaleByteProgress(read, total int64) (done, tot int) {
	if total < 0 {
		total = 0
	}
	if read < 0 {
		read = 0
	}
	if total > 0 && read > total {
		read = total
	}
	scale := int64(1)
	if total > flashbackProgressIntMax {
		scale = total/flashbackProgressIntMax + 1
	}
	tot = int(total / scale)
	done = int(read / scale)
	if tot < 1 && total > 0 {
		tot = 1
	}
	return done, tot
}

func flashbackFormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	if n >= 1<<20 {
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	}
	if n >= 1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

// flashbackMySQLPickStartFile 从新到旧探测 binlog 首事件时间，定位包含 target_time 的起始文件。
func flashbackMySQLPickStartFile(ctx context.Context, creds flashbackMySQLCreds, logs []flashbackMySQLBinlogFile, target time.Time) int {
	if len(logs) == 0 {
		return 0
	}
	times := make([]time.Time, len(logs))
	probes := 0
	for i := len(logs) - 1; i >= 0 && probes < flashbackMySQLMaxFileProbes; i-- {
		ts, err := flashbackMySQLFirstEventTime(ctx, creds, logs[i].Name)
		probes++
		if err != nil {
			continue
		}
		times[i] = ts
		if !ts.After(target) {
			return i
		}
	}
	return flashbackMySQLPickStartIndexByTimes(times, target)
}

func flashbackMySQLProbeDump(ctx context.Context, creds flashbackMySQLCreds, file string, pos uint32) error {
	file = strings.TrimSpace(file)
	if file == "" {
		return fmt.Errorf("empty binlog file")
	}
	if pos < 4 {
		pos = 4
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, flashbackMySQLDumpProbeTimeout)
	defer cancel()
	dumpID := flashbackMySQLDumpServerID("precheck-probe:" + creds.Host + ":" + strconv.Itoa(creds.Port))
	cfg := flashbackMySQLDumpSyncerConfig(creds, dumpID)
	syncer := replication.NewBinlogSyncer(cfg)
	defer syncer.Close()
	streamer, err := syncer.StartSync(gomysql.Position{Name: file, Pos: pos})
	if err != nil {
		return err
	}
	_, err = streamer.GetEvent(ctx)
	return err
}

// flashbackMySQLEventWindow 按 binlog 秒级时间戳判断事件是否在 [target, end] 内。
// skip=true 表示不生成 SQL；stop=true 表示已越过结束时间，应停止 DUMP。
func flashbackMySQLEventWindow(ts, targetUnix, endUnix uint32) (skip, stop bool) {
	if ts != 0 && ts > endUnix {
		return true, true
	}
	if ts != 0 && ts < targetUnix {
		return true, false
	}
	return false, false
}

type flashbackMySQLDumpOpt struct {
	StartFile string
	StartPos  uint32
	EndFile   string
	EndPos    uint32
	Target    time.Time
	End       time.Time
	StartXID  int64
	StopXID   int64
	MaxBytes  int64
}

func flashbackDumpMySQLBinlog(
	ctx context.Context,
	creds flashbackMySQLCreds,
	serverID uint32,
	opt flashbackMySQLDumpOpt,
	dict *flashbackMySQLDict,
	progress func(file string, readBytes int64, logPos uint32),
	handle func(flashbackChange) bool,
) (flashbackMySQLDumpStat, error) {
	var st flashbackMySQLDumpStat
	startFile := strings.TrimSpace(opt.StartFile)
	startPos := flashbackMySQLNormalizeStartPos(opt.StartPos)
	st.StartFile = startFile
	openDump := func(sid uint32, file string, pos uint32) (*replication.BinlogSyncer, *replication.BinlogStreamer, error) {
		syncer := replication.NewBinlogSyncer(flashbackMySQLDumpSyncerConfig(creds, sid))
		streamer, err := syncer.StartSync(gomysql.Position{Name: file, Pos: pos})
		if err != nil {
			syncer.Close()
			return nil, nil, err
		}
		return syncer, streamer, nil
	}
	syncer, streamer, err := openDump(serverID, startFile, startPos)
	if err != nil {
		return st, flashbackMySQLDumpPublicErr(err)
	}
	defer func() { syncer.Close() }()

	tables := map[uint64]*replication.TableMapEvent{}
	curFile := startFile
	resumePos := startPos
	outerLeft := flashbackMySQLDumpOuterRetry
	targetUnix := uint32(opt.Target.Unix())
	endUnix := uint32(opt.End.Unix())
	if endUnix == 0 {
		endUnix = uint32(time.Now().Unix())
	}
	var lastProgress time.Time
	var skipBytes int64
	var pending []flashbackChange
	emit := func(ch flashbackChange) bool {
		xid := flashbackChangeXID(ch)
		if !flashbackMySQLXIDInRange(xid, opt.StartXID, opt.StopXID) {
			return !flashbackMySQLXIDPastStop(xid, opt.StopXID)
		}
		if handle != nil && !handle(ch) {
			st.ChangeTrunc = true
			return false
		}
		return !flashbackMySQLXIDPastStop(xid, opt.StopXID)
	}
	flushPending := func(xid int64) bool {
		for i := range pending {
			pending[i].XID64 = xid
			if !emit(pending[i]) {
				pending = pending[:0]
				return false
			}
		}
		pending = pending[:0]
		return !flashbackMySQLXIDPastStop(xid, opt.StopXID)
	}
	for {
		ev, err := streamer.GetEvent(ctx)
		if err != nil {
			if ctx.Err() != nil {
				_ = flushPending(0)
				return st, ctx.Err()
			}
			if outerLeft > 0 && flashbackMySQLDumpTransient(err) {
				outerLeft--
				syncer.Close()
				sid := serverID + uint32(flashbackMySQLDumpOuterRetry-outerLeft)
				var oerr error
				syncer, streamer, oerr = openDump(sid, curFile, resumePos)
				if oerr != nil {
					_ = flushPending(0)
					return st, flashbackMySQLDumpPublicErr(oerr)
				}
				select {
				case <-ctx.Done():
					_ = flushPending(0)
					return st, ctx.Err()
				case <-time.After(time.Second):
				}
				continue
			}
			_ = flushPending(0)
			return st, flashbackMySQLDumpPublicErr(err)
		}
		if ev == nil || ev.Header == nil {
			continue
		}
		if ev.Header.LogPos > 0 {
			resumePos = ev.Header.LogPos
		}
		st.Events++
		evSize := int64(ev.Header.EventSize)
		ts := ev.Header.Timestamp
		if ts == 0 || ts >= targetUnix {
			st.Bytes += evSize
			if opt.MaxBytes > 0 && st.Bytes > opt.MaxBytes {
				st.ByteTrunc = true
				_ = flushPending(0)
				return st, nil
			}
		} else {
			skipBytes += evSize
			if opt.MaxBytes > 0 && skipBytes > opt.MaxBytes*2 {
				return st, fmt.Errorf("扫描时间窗之前的 binlog 已超过体积上限，请缩小时间范围或确认 binlog 未堆积")
			}
		}
		if ev.Header.EventType == replication.ROTATE_EVENT {
			if re, ok := ev.Event.(*replication.RotateEvent); ok {
				curFile = string(re.NextLogName)
				st.StopFile = curFile
			}
		}
		if progress != nil && (lastProgress.IsZero() || time.Since(lastProgress) > 2*time.Second) {
			progress(curFile, skipBytes+st.Bytes, ev.Header.LogPos)
			lastProgress = time.Now()
		}
		if ev.Header.EventType == replication.ROTATE_EVENT {
			continue
		}
		if flashbackMySQLPosReached(curFile, ev.Header.LogPos, opt.EndFile, opt.EndPos) {
			st.StopFile = curFile
			_ = flushPending(0)
			return st, nil
		}
		skip, stop := flashbackMySQLEventWindow(ts, targetUnix, endUnix)
		if stop {
			st.StopFile = curFile
			_ = flushPending(0)
			return st, nil
		}
		switch ev.Header.EventType {
		case replication.TABLE_MAP_EVENT:
			if tm, ok := ev.Event.(*replication.TableMapEvent); ok {
				tables[tm.TableID] = tm
			}
		case replication.QUERY_EVENT:
			if qe, ok := ev.Event.(*replication.QueryEvent); ok {
				q := string(qe.Query)
				switch flashbackMySQLTxnBoundary(q) {
				case "begin":
					if !flushPending(0) {
						st.StopFile = curFile
						return st, nil
					}
				case "rollback":
					pending = pending[:0]
				case "commit":
					if !flushPending(0) {
						st.StopFile = curFile
						return st, nil
					}
				default:
					if op := flashbackMySQLDDLOp(q); op != "" {
						if ts >= targetUnix {
							st.DDLSkipped++
						}
						if skip {
							continue
						}
						ch := flashbackChange{
							TS:      time.Unix(int64(ts), 0),
							Schema:  string(qe.Schema),
							Op:      op,
							MySQL:   true,
							DDLRedo: strings.TrimRight(strings.TrimSpace(q), ";") + ";",
							DDLRisk: "MySQL 仅审计原文，不做反向闪回",
						}
						if !emit(ch) {
							st.StopFile = curFile
							return st, nil
						}
					}
				}
			}
		case replication.XID_EVENT:
			xid := int64(0)
			if xe, ok := ev.Event.(*replication.XIDEvent); ok {
				xid = int64(xe.XID)
			}
			if skip {
				pending = pending[:0]
				if flashbackMySQLXIDPastStop(xid, opt.StopXID) {
					st.StopFile = curFile
					return st, nil
				}
				continue
			}
			if !flushPending(xid) {
				st.StopFile = curFile
				return st, nil
			}
		case replication.WRITE_ROWS_EVENTv0, replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2,
			replication.UPDATE_ROWS_EVENTv0, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2,
			replication.PARTIAL_UPDATE_ROWS_EVENT,
			replication.DELETE_ROWS_EVENTv0, replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
			if skip {
				continue
			}
			re, ok := ev.Event.(*replication.RowsEvent)
			if !ok || re == nil {
				continue
			}
			tm := re.Table
			if tm == nil {
				tm = tables[re.TableID]
			}
			if tm == nil {
				continue
			}
			schema := string(tm.Schema)
			table := string(tm.Table)
			rel := dict.match(schema, table)
			if rel == nil {
				continue
			}
			op := flashbackMySQLEventOp(ev)
			if op == "" {
				continue
			}
			pk := rel.PKCols
			noPK := rel.NoPK || len(pk) == 0
			if noPK {
				pk = nil
			}
			rows := re.Rows
			step := 1
			if op == "UPDATE" {
				step = 2
			}
			for i := 0; i+step-1 < len(rows); i += step {
				ch := flashbackChange{
					TS:     time.Unix(int64(ts), 0),
					Schema: schema,
					Table:  table,
					Op:     op,
					PKCols: pk,
					NoPK:   noPK,
					MySQL:  true,
				}
				switch op {
				case "INSERT":
					ch.New = flashbackMySQLRowMap(rel, rows[i], tm)
				case "DELETE":
					ch.Old = flashbackMySQLRowMap(rel, rows[i], tm)
				case "UPDATE":
					ch.Old = flashbackMySQLRowMap(rel, rows[i], tm)
					ch.New = flashbackMySQLRowMap(rel, rows[i+1], tm)
				}
				st.Rows++
				pending = append(pending, ch)
				if len(pending) > flashbackMySQLMaxPendingRows {
					return st, fmt.Errorf("单事务变更超过 %d 行，已停止以免占满内存。请缩小时间窗或按表拆分", flashbackMySQLMaxPendingRows)
				}
			}
		}
	}
}
