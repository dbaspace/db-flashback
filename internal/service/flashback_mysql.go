package service

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	mdmmodel "db-flashback/internal/mdmmodel"

	"db-flashback/internal/service/dto"
)

const defaultMySQLPort = 3306

const (
	flashbackParseModeOnline = "online" // MySQL：BINLOG DUMP，日志留在实例
	flashbackParseModeFile   = "file"   // PG 自建：把 WAL 拉到 Hub 再解析
	flashbackParseModeCloud  = "cloud"  // PG 云库：按时间窗下载增量日志备份再解析
)

func flashbackIsMySQL(dbType any) bool {
	return strings.EqualFold(strings.TrimSpace(fmt.Sprint(dbType)), string(mdmmodel.MySQLDBTypeEnum))
}

func flashbackIsPostgres(dbType any) bool {
	s := strings.ToLower(strings.TrimSpace(fmt.Sprint(dbType)))
	return s == string(mdmmodel.PostgreSQLDBTypeEnum) || s == "postgres" || s == "postgresql"
}

type flashbackMySQLCreds struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
}

func flashbackMySQLDSN(c flashbackMySQLCreds) string {
	cfg := mysqldriver.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.DBName = c.DBName
	cfg.ParseTime = true
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	cfg.Loc = time.Local
	return cfg.FormatDSN()
}

func connectSourceMySQL(ctx context.Context, instanceID, dbName, fallbackHost string, fallbackPort int) (*sql.DB, *mdmmodel.ResourceDbsInfo, flashbackMySQLCreds, error) {
	creds, res, err := flashbackResolveMySQLCreds(ctx, instanceID, dbName, fallbackHost, fallbackPort)
	if err != nil {
		return nil, res, creds, err
	}
	db, err := sql.Open("mysql", flashbackMySQLDSN(creds))
	if err != nil {
		return nil, res, creds, fmt.Errorf("sql.Open mysql: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if flashbackMySQLUnknownDatabase(err) && !strings.EqualFold(creds.DBName, "mysql") {
			fallback := creds
			fallback.DBName = "mysql"
			db, err = sql.Open("mysql", flashbackMySQLDSN(fallback))
			if err != nil {
				return nil, res, creds, fmt.Errorf("sql.Open mysql: %w", err)
			}
			if perr := db.PingContext(ctx); perr != nil {
				_ = db.Close()
				return nil, res, creds, fmt.Errorf("ping mysql: %w", perr)
			}
			return db, res, creds, nil
		}
		return nil, res, creds, fmt.Errorf("ping mysql: %w", err)
	}
	return db, res, creds, nil
}

func flashbackMySQLUnknownDatabase(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Unknown database") || strings.Contains(s, "1049")
}

// flashbackMySQLCredsFromEnv 自测未纳管实例时用环境变量覆盖凭据，不把密码写进仓库。
func flashbackMySQLCredsFromEnv(fallbackHost string, fallbackPort int, dbName string) (flashbackMySQLCreds, bool) {
	user := strings.TrimSpace(os.Getenv("FLASHBACK_MYSQL_USER"))
	pass := os.Getenv("FLASHBACK_MYSQL_PASSWORD")
	if user == "" || pass == "" {
		return flashbackMySQLCreds{}, false
	}
	host := strings.TrimSpace(os.Getenv("FLASHBACK_MYSQL_HOST"))
	if host == "" {
		host = strings.TrimSpace(fallbackHost)
	}
	port := fallbackPort
	if raw := strings.TrimSpace(os.Getenv("FLASHBACK_MYSQL_PORT")); raw != "" {
		if p, err := strconv.Atoi(raw); err == nil && p > 0 {
			port = p
		}
	}
	if port <= 0 {
		port = defaultMySQLPort
	}
	if strings.TrimSpace(dbName) == "" {
		dbName = strings.TrimSpace(os.Getenv("FLASHBACK_MYSQL_DB"))
	}
	if dbName == "" {
		dbName = "fbtest"
	}
	return flashbackMySQLCreds{Host: host, Port: port, User: user, Password: pass, DBName: dbName}, true
}

func flashbackResolveMySQLCreds(ctx context.Context, instanceID, dbName, fallbackHost string, fallbackPort int) (flashbackMySQLCreds, *mdmmodel.ResourceDbsInfo, error) {
	var zero flashbackMySQLCreds
	if creds, ok := flashbackMySQLCredsFromEnv(fallbackHost, fallbackPort, dbName); ok {
		return creds, nil, nil
	}
	inst, err := resolveConnectInstance(instanceID, fallbackHost, fallbackPort)
	if err != nil {
		return zero, nil, err
	}
	res := instanceToResource(inst)
	if !flashbackIsMySQL(inst.DBType) {
		return zero, res, fmt.Errorf("instance %s is not MySQL (db_type=%s)", instanceID, inst.DBType)
	}
	target := strings.TrimSpace(dbName)
	if target == "" {
		target = "mysql"
	}
	port := inst.Port
	if port <= 0 {
		port = defaultMySQLPort
	}
	if strings.TrimSpace(inst.User) == "" || inst.Password == "" {
		return zero, res, fmt.Errorf("instance %s missing credential", instanceID)
	}
	return flashbackMySQLCreds{
		Host:     strings.TrimSpace(inst.Host),
		Port:     port,
		User:     strings.TrimSpace(inst.User),
		Password: inst.Password,
		DBName:   target,
	}, res, nil
}

func flashbackParseMySQLTable(raw, defaultDB string) (dbName, table string, err error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "`\"")
	if raw == "" {
		return "", "", fmt.Errorf("empty table name")
	}
	parts := strings.Split(raw, ".")
	switch len(parts) {
	case 1:
		dbName = strings.TrimSpace(defaultDB)
		if dbName == "" {
			return "", "", fmt.Errorf("table %q 缺少库名", raw)
		}
		return dbName, strings.Trim(parts[0], "`\""), nil
	case 2:
		return strings.Trim(parts[0], "`\""), strings.Trim(parts[1], "`\""), nil
	default:
		return "", "", fmt.Errorf("invalid table %q, expect db.table", raw)
	}
}

func flashbackMySQLDumpServerID(taskID string, avoid ...uint32) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("hub-flashback:" + taskID))
	id := h.Sum32()%100000000 + 100000000
	for _, a := range avoid {
		if id == a {
			id++
		}
	}
	if id == 0 {
		id = 100000001
	}
	return id
}

func flashbackMySQLVar(ctx context.Context, db *sql.DB, name string) string {
	if db == nil {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "`';\"\\ \t") {
		return ""
	}
	var raw any
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL."+name).Scan(&raw); err == nil && raw != nil {
		return flashbackMySQLVarString(raw)
	}
	var n string
	var v sql.NullString
	if err := db.QueryRowContext(ctx, "SHOW VARIABLES LIKE '"+name+"'").Scan(&n, &v); err == nil && v.Valid {
		return strings.TrimSpace(v.String)
	}
	return ""
}

func flashbackMySQLVarString(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case []byte:
		return strings.TrimSpace(string(v))
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func flashbackMySQLFormatGate(format, rowImage string) (status, msg string) {
	f := strings.ToLower(strings.TrimSpace(format))
	img := strings.ToLower(strings.TrimSpace(rowImage))
	if f != "row" {
		return flashbackCheckFailed, fmt.Sprintf("当前 binlog_format=%s，一期仅支持 ROW（STATEMENT/MIXED 无行镜像，无法生成可靠回滚 SQL）", format)
	}
	if img != "full" {
		return flashbackCheckFailed, fmt.Sprintf("当前 binlog_row_image=%s，一期仅支持 FULL（MINIMAL/NOBLOB 无法可靠拼回滚 SQL）", rowImage)
	}
	return flashbackCheckPassed, "ROW + FULL"
}

func flashbackMySQLHasReplPriv(grants []string) (slave, client bool) {
	for _, g := range grants {
		u := strings.ToUpper(g)
		star := strings.Contains(u, "ON *.*")
		if star && strings.Contains(u, "ALL PRIVILEGES") {
			return true, true
		}
		if strings.Contains(u, "REPLICATION SLAVE") || strings.Contains(u, "REPLICATION_SLAVE_ADMIN") {
			slave = true
		}
		if strings.Contains(u, "REPLICATION CLIENT") || strings.Contains(u, "BINLOG MONITOR") || strings.Contains(u, "BINLOG_ADMIN") {
			client = true
		}
	}
	return slave, client
}

func flashbackMySQLShowGrants(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		rows, err = db.QueryContext(ctx, "SHOW GRANTS")
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type flashbackMySQLBinlogFile struct {
	Name string
	Size int64
}

func flashbackMySQLListBinlogs(ctx context.Context, db *sql.DB) ([]flashbackMySQLBinlogFile, error) {
	rows, err := db.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []flashbackMySQLBinlogFile
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		f := flashbackMySQLBinlogFile{Name: vals[0].String}
		if len(vals) > 1 {
			f.Size, _ = strconv.ParseInt(vals[1].String, 10, 64)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func flashbackMySQLMasterStatus(ctx context.Context, db *sql.DB) (file string, pos uint32, err error) {
	for _, q := range []string{"SHOW MASTER STATUS", "SHOW BINARY LOG STATUS"} {
		rows, qerr := db.QueryContext(ctx, q)
		if qerr != nil {
			err = qerr
			continue
		}
		cols, cerr := rows.Columns()
		if cerr != nil {
			_ = rows.Close()
			err = cerr
			continue
		}
		if !rows.Next() {
			err = rows.Err()
			if err == nil {
				err = sql.ErrNoRows
			}
			_ = rows.Close()
			continue
		}
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if serr := rows.Scan(ptrs...); serr != nil {
			_ = rows.Close()
			err = serr
			continue
		}
		file = vals[0].String
		if len(vals) > 1 {
			n, _ := strconv.ParseUint(vals[1].String, 10, 32)
			pos = uint32(n)
		}
		_ = rows.Close()
		return file, pos, nil
	}
	if err == nil {
		err = fmt.Errorf("SHOW MASTER STATUS / SHOW BINARY LOG STATUS 均失败")
	}
	return "", 0, err
}

func flashbackMySQLExpire(ctx context.Context, db *sql.DB) time.Duration {
	if sec := flashbackMySQLVar(ctx, db, "binlog_expire_logs_seconds"); sec != "" && sec != "0" {
		n, err := strconv.ParseInt(sec, 10, 64)
		if err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if days := flashbackMySQLVar(ctx, db, "expire_logs_days"); days != "" && days != "0" {
		n, err := strconv.ParseInt(days, 10, 64)
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return 0
}

func flashbackMySQLPickStartIndex(n int, target, now time.Time, expire time.Duration) int {
	if n <= 1 || expire <= 0 {
		return 0
	}
	spanStart := now.Add(-expire)
	if !target.After(spanStart) {
		return 0
	}
	frac := target.Sub(spanStart).Seconds() / expire.Seconds()
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	idx := int(frac * float64(n))
	if idx >= n {
		idx = n - 1
	}
	if idx > 2 {
		idx -= 2
	} else {
		idx = 0
	}
	return idx
}

// flashbackMySQLPickStartIndexByTimes 按各 binlog 首事件时间选起始文件：
// 取最后一个 first_ts <= target 的下标；全部晚于 target 则取最早已知文件；全未知则 0。
func flashbackMySQLPickStartIndexByTimes(firstTS []time.Time, target time.Time) int {
	lastLE := -1
	firstKnown := -1
	for i, ts := range firstTS {
		if ts.IsZero() {
			continue
		}
		if firstKnown < 0 {
			firstKnown = i
		}
		if !ts.After(target) {
			lastLE = i
		}
	}
	if lastLE >= 0 {
		return lastLE
	}
	if firstKnown >= 0 {
		return firstKnown
	}
	return 0
}

func flashbackMySQLFileLater(a, b string) bool {
	return strings.TrimSpace(a) > strings.TrimSpace(b)
}

func flashbackValidateBinlogName(name, field string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("%s 非法", field)
	}
	return nil
}

func flashbackMySQLNormalizeStartPos(pos uint32) uint32 {
	if pos < 4 {
		return 4
	}
	return pos
}

func flashbackMySQLFindBinlog(logs []flashbackMySQLBinlogFile, name string) int {
	name = strings.TrimSpace(name)
	if name == "" {
		return -1
	}
	for i, f := range logs {
		if f.Name == name {
			return i
		}
	}
	return -1
}

func flashbackMySQLResolveDumpRange(
	startFile string, startPos uint32,
	stopFile string, stopPos uint32,
	logs []flashbackMySQLBinlogFile,
	pickedStartFile, masterFile string, masterPos uint32,
) (file string, pos uint32, endFile string, endPos uint32, err error) {
	file = strings.TrimSpace(startFile)
	pos = flashbackMySQLNormalizeStartPos(startPos)
	if file == "" {
		file = strings.TrimSpace(pickedStartFile)
		pos = 4
	} else if flashbackMySQLFindBinlog(logs, file) < 0 {
		return "", 0, "", 0, fmt.Errorf("start_file %s 不在当前 SHOW BINARY LOGS 中", file)
	}
	endFile = strings.TrimSpace(stopFile)
	endPos = stopPos
	if endFile == "" {
		endFile = strings.TrimSpace(masterFile)
		endPos = masterPos
	} else if flashbackMySQLFindBinlog(logs, endFile) < 0 {
		return "", 0, "", 0, fmt.Errorf("stop_file %s 不在当前 SHOW BINARY LOGS 中", endFile)
	} else if endPos == 0 {
		endPos = ^uint32(0)
	}
	if file == "" {
		return "", 0, "", 0, fmt.Errorf("无法确定 BINLOG DUMP 起始文件")
	}
	return file, pos, endFile, endPos, nil
}

func flashbackMySQLXIDInRange(xid, start, stop int64) bool {
	if start > 0 && (xid == 0 || xid < start) {
		return false
	}
	if stop > 0 && xid > 0 && xid > stop {
		return false
	}
	return true
}

func flashbackMySQLXIDPastStop(xid, stop int64) bool {
	return stop > 0 && xid > 0 && xid > stop
}

func flashbackMySQLPosReached(curFile string, curPos uint32, endFile string, endPos uint32) bool {
	if endFile == "" {
		return false
	}
	if flashbackMySQLFileLater(curFile, endFile) {
		return true
	}
	return curFile == endFile && curPos >= endPos
}

func flashbackMySQLHostingStatus() (status, msg string) {
	return flashbackCheckPassed, "MySQL 统一在线模式：不按云厂商域名区分，binlog 留在实例上通过 BINLOG DUMP 解析"
}

// flashbackMySQLRejectOfflineWorkDir MySQL 一律在线 DUMP，禁止把 binlog 落到 Hub。
func flashbackMySQLRejectOfflineWorkDir(workDir string) error {
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	return fmt.Errorf("MySQL 仅支持在线模式，禁止将 binlog 落到 Hub")
}

func flashbackPrecheckMySQL(ctx context.Context, db *sql.DB, req *dto.FlashbackTaskReq, out *dto.FlashbackPrecheckResult, target, end time.Time) (*dto.FlashbackPrecheckResult, error) {
	ver := flashbackMySQLVar(ctx, db, "version")
	st, msg := flashbackMySQLHostingStatus()
	flashbackAddCheck(&out.Items, "selfhosted", "实例来源", st, msg)
	out.ParseMode = flashbackParseModeOnline
	flashbackAddCheck(&out.Items, "parse_mode", "在线模式", flashbackCheckPassed,
		"binlog 留在实例上，Hub 只 DUMP、不落本地文件")
	flashbackAddCheck(&out.Items, "dict_snapshot", "数据字典快照", flashbackCheckPassed,
		"任务创建时冻结 information_schema 列序到 dict.json，解析优先用该快照（可用 dict_task_id 复用）")
	flashbackAddCheck(&out.Items, "time_window", "时间窗", flashbackCheckPassed,
		fmt.Sprintf("仅解析 %s ~ %s 内的事件，不会处理实例全部 binlog",
			flashbackFormatLocalTime(target), flashbackFormatLocalTime(end)))

	logBin := flashbackMySQLVar(ctx, db, "log_bin")
	format := flashbackMySQLVar(ctx, db, "binlog_format")
	rowImage := flashbackMySQLVar(ctx, db, "binlog_row_image")
	serverID := flashbackMySQLVar(ctx, db, "server_id")
	out.ServerVersion = ver
	out.WALLevel = format
	expire := flashbackMySQLExpire(ctx, db)
	if expire > 0 {
		out.ArchiveMode = expire.String()
	} else {
		out.ArchiveMode = "manual_purge"
	}
	flashbackAddCheck(&out.Items, "version", "MySQL 版本", flashbackCheckPassed, fmt.Sprintf("%s server_id=%s", ver, serverID))

	lb := strings.ToLower(logBin)
	if lb != "on" && lb != "1" {
		flashbackAddCheck(&out.Items, "log_bin", "log_bin", flashbackCheckFailed, fmt.Sprintf("当前 log_bin=%s，未开启 binlog，无法闪回", logBin))
		out.OK = false
		return out, nil
	}
	flashbackAddCheck(&out.Items, "log_bin", "log_bin", flashbackCheckPassed, logBin)

	if st, msg := flashbackMySQLFormatGate(format, rowImage); st == flashbackCheckFailed {
		flashbackAddCheck(&out.Items, "binlog_format", "binlog_format / row_image", st, msg)
		out.OK = false
		return out, nil
	} else {
		flashbackAddCheck(&out.Items, "binlog_format", "binlog_format / row_image", st, msg)
	}

	if flashbackWantDDL(req.SQLType) {
		flashbackAddCheck(&out.Items, "sql_type", "sql_type", flashbackCheckWarning,
			"MySQL DDL 仅落 Query 原文审计，不生成反向闪回 SQL")
	}

	grants, gerr := flashbackMySQLShowGrants(ctx, db)
	if gerr != nil {
		flashbackAddCheck(&out.Items, "grants", "复制权限", flashbackCheckFailed, "SHOW GRANTS 失败："+gerr.Error())
		out.OK = false
		return out, nil
	}
	slave, client := flashbackMySQLHasReplPriv(grants)
	if !slave || !client {
		flashbackAddCheck(&out.Items, "grants", "复制权限", flashbackCheckFailed,
			"账号缺少 REPLICATION SLAVE / REPLICATION CLIENT（或 8.0+ 的 BINLOG MONITOR / REPLICATION_SLAVE_ADMIN）")
		out.OK = false
		return out, nil
	}
	flashbackAddCheck(&out.Items, "grants", "复制权限", flashbackCheckPassed, "具备 BINLOG DUMP / SHOW BINARY LOGS 权限")

	dict, err := flashbackLoadMySQLDictionary(ctx, db, req.Database, req.Tables)
	if err != nil {
		name := "指定表"
		if flashbackTablesIsAll(req.Tables) {
			name = "整库表"
		}
		flashbackAddCheck(&out.Items, "tables", name, flashbackCheckFailed, err.Error())
		out.OK = false
		return out, nil
	}
	var missing, noPK, dbMismatch []string
	for _, rel := range dict.Wanted {
		if rel == nil {
			continue
		}
		if rel.Missing {
			missing = append(missing, rel.Schema+"."+rel.Name)
			continue
		}
		if rel.NoPK {
			noPK = append(noPK, rel.Schema+"."+rel.Name)
		}
		if req.Database != "" && !strings.EqualFold(rel.Schema, req.Database) {
			dbMismatch = append(dbMismatch, rel.Schema+"."+rel.Name)
		}
	}
	if len(missing) > 0 {
		flashbackAddCheck(&out.Items, "tables", "指定表", flashbackCheckFailed,
			"下列表当前不存在："+strings.Join(missing, ", ")+"。MySQL 一期只做 DML，表必须仍存在")
		out.OK = false
		return out, nil
	}
	flashbackAddTableScopeCheck(&out.Items, req.Tables, len(dict.Wanted))
	if len(noPK) > 0 {
		flashbackAddCheck(&out.Items, "primary_key", "主键", flashbackCheckWarning,
			"下列表无主键，回滚 SQL 将使用全列等值，存在误更新/误删风险："+strings.Join(noPK, ", "))
	} else {
		flashbackAddCheck(&out.Items, "primary_key", "主键", flashbackCheckPassed, "指定表均有主键")
	}
	if len(dbMismatch) > 0 {
		flashbackAddCheck(&out.Items, "database", "库名", flashbackCheckWarning,
			"tables 中的库名与 database 不一致："+strings.Join(dbMismatch, ", "))
	}

	logs, lerr := flashbackMySQLListBinlogs(ctx, db)
	if lerr != nil {
		flashbackAddCheck(&out.Items, "binary_logs", "SHOW BINARY LOGS", flashbackCheckFailed, lerr.Error())
		out.OK = false
		return out, nil
	}
	if len(logs) == 0 {
		flashbackAddCheck(&out.Items, "binary_logs", "SHOW BINARY LOGS", flashbackCheckFailed, "实例无可用 binlog 文件")
		out.OK = false
		return out, nil
	}
	curFile, _, merr := flashbackMySQLMasterStatus(ctx, db)
	if merr != nil {
		flashbackAddCheck(&out.Items, "master_status", "MASTER STATUS", flashbackCheckWarning, merr.Error())
	} else {
		flashbackAddCheck(&out.Items, "master_status", "MASTER STATUS", flashbackCheckPassed, curFile)
	}

	now := time.Now()
	from := target
	to := end
	if expire > 0 {
		retainFrom := now.Add(-expire)
		if target.Before(retainFrom.Add(-2 * time.Hour)) {
			out.WALFrom = &retainFrom
			out.WALTo = &to
			out.Covered = false
			flashbackAddCheck(&out.Items, "coverage", "binlog 时间覆盖", flashbackCheckFailed,
				fmt.Sprintf("target_time 早于当前 binlog 过期策略覆盖范围（约 %s ~ %s），文件可能已被 purge", flashbackFormatLocalTime(retainFrom), flashbackFormatLocalTime(now)))
			out.OK = false
			return out, nil
		}
	}
	out.WALFrom = &from
	out.WALTo = &to

	probeFile := curFile
	if probeFile == "" && len(logs) > 0 {
		probeFile = logs[len(logs)-1].Name
	}
	creds, _, cerr := flashbackResolveMySQLCreds(ctx, req.InstanceID, req.Database, out.Host, out.Port)
	if cerr != nil {
		flashbackAddCheck(&out.Items, "binlog_dump", "BINLOG DUMP", flashbackCheckFailed,
			"无法取得 DUMP 凭据："+cerr.Error())
		out.OK = false
		return out, nil
	}
	probePos := uint32(4)
	if req.StartFile != "" {
		if flashbackMySQLFindBinlog(logs, req.StartFile) < 0 {
			flashbackAddCheck(&out.Items, "start_file", "start_file", flashbackCheckFailed,
				fmt.Sprintf("%s 不在当前 SHOW BINARY LOGS 中（可能已 purge）", req.StartFile))
			out.OK = false
			return out, nil
		}
		probeFile = req.StartFile
		probePos = flashbackMySQLNormalizeStartPos(req.StartPos)
		flashbackAddCheck(&out.Items, "start_file", "start_file / start_pos", flashbackCheckPassed,
			fmt.Sprintf("%s:%d", req.StartFile, probePos))
	}
	if req.StopFile != "" {
		if flashbackMySQLFindBinlog(logs, req.StopFile) < 0 {
			flashbackAddCheck(&out.Items, "stop_file", "stop_file", flashbackCheckFailed,
				fmt.Sprintf("%s 不在当前 SHOW BINARY LOGS 中", req.StopFile))
			out.OK = false
			return out, nil
		}
		flashbackAddCheck(&out.Items, "stop_file", "stop_file / stop_pos", flashbackCheckPassed,
			fmt.Sprintf("%s:%d", req.StopFile, req.StopPos))
	}
	if perr := flashbackMySQLProbeDump(ctx, creds, probeFile, probePos); perr != nil {
		flashbackAddCheck(&out.Items, "binlog_dump", "BINLOG DUMP", flashbackCheckFailed, flashbackMySQLDumpProbeMessage(perr))
		out.OK = false
		return out, nil
	}
	flashbackAddCheck(&out.Items, "binlog_dump", "BINLOG DUMP", flashbackCheckPassed, "BINLOG DUMP 可用")

	startIdx := 0
	if req.StartFile != "" {
		startIdx = flashbackMySQLFindBinlog(logs, req.StartFile)
	} else {
		startIdx = flashbackMySQLPickStartFile(ctx, creds, logs, target)
		if startIdx < 0 || startIdx >= len(logs) {
			startIdx = flashbackMySQLPickStartIndex(len(logs), target, now, expire)
		}
	}
	if startIdx < 0 || startIdx >= len(logs) {
		startIdx = 0
	}
	picked := logs[startIdx:]
	if req.StopFile != "" {
		if stopIdx := flashbackMySQLFindBinlog(logs, req.StopFile); stopIdx >= startIdx {
			picked = logs[startIdx : stopIdx+1]
		}
	}
	var winBytes int64
	for _, f := range picked {
		winBytes += f.Size
	}
	maxBytes := flashbackMaxWALBytes(ctx)
	truncated := winBytes > maxBytes
	if truncated {
		winBytes = maxBytes
	}
	out.WALFiles = len(picked)
	out.WALBytes = winBytes
	out.Covered = true
	startLabel := picked[0].Name
	if req.StartFile != "" {
		startLabel = fmt.Sprintf("%s:%d", req.StartFile, flashbackMySQLNormalizeStartPos(req.StartPos))
	}
	msg = fmt.Sprintf("按时间窗 %s ~ %s 从 %s 起 DUMP（约 %d 个文件 / %d bytes），窗前事件跳过、窗后停止",
		flashbackFormatLocalTime(target), flashbackFormatLocalTime(end), startLabel, len(picked), winBytes)
	if truncated {
		flashbackAddCheck(&out.Items, "coverage", "binlog 时间覆盖", flashbackCheckWarning, msg+"（超过体积上限，解析时将截断）")
	} else {
		flashbackAddCheck(&out.Items, "coverage", "binlog 时间覆盖", flashbackCheckPassed, msg)
	}

	out.OK = true
	for _, it := range out.Items {
		if it.Status == flashbackCheckFailed {
			out.OK = false
			break
		}
	}
	return out, nil
}

type flashbackMySQLCol struct {
	Name       string
	DataType   string
	ColumnType string
	Charset    string
}

type flashbackMySQLRel struct {
	Schema  string
	Name    string
	Columns []flashbackMySQLCol
	PKCols  []string
	NoPK    bool
	Missing bool
}

type flashbackMySQLDict struct {
	DBName string
	Wanted map[string]*flashbackMySQLRel
}

func (d *flashbackMySQLDict) match(schema, table string) *flashbackMySQLRel {
	if d == nil {
		return nil
	}
	key := strings.ToLower(schema + "." + table)
	if rel := d.Wanted[key]; rel != nil {
		return rel
	}
	if d.DBName != "" && strings.EqualFold(schema, d.DBName) {
		return d.Wanted[strings.ToLower(d.DBName+"."+table)]
	}
	return nil
}

func flashbackListMySQLUserTables(ctx context.Context, db *sql.DB, schema string) ([]string, error) {
	schema = strings.TrimSpace(schema)
	if schema == "" {
		return nil, fmt.Errorf("database 必填")
	}
	rows, err := db.QueryContext(ctx, `
SELECT TABLE_NAME
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
ORDER BY TABLE_NAME`, schema)
	if err != nil {
		return nil, fmt.Errorf("list user tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		out = append(out, schema+"."+table)
	}
	return out, rows.Err()
}

func flashbackLoadMySQLDictionary(ctx context.Context, db *sql.DB, defaultDB string, tables []string) (*flashbackMySQLDict, error) {
	names := flashbackNormalizeTableNames(tables)
	if len(names) == 0 {
		listed, err := flashbackListMySQLUserTables(ctx, db, defaultDB)
		if err != nil {
			return nil, err
		}
		if len(listed) == 0 {
			return nil, fmt.Errorf("库下没有可闪回的表")
		}
		names = listed
	}
	dict := &flashbackMySQLDict{DBName: defaultDB, Wanted: map[string]*flashbackMySQLRel{}}
	for _, raw := range names {
		schema, table, err := flashbackParseMySQLTable(raw, defaultDB)
		if err != nil {
			return nil, err
		}
		rel, err := flashbackLoadMySQLRelation(ctx, db, schema, table)
		if err != nil {
			if !strings.Contains(err.Error(), "不存在") {
				return nil, err
			}
			rel = &flashbackMySQLRel{Schema: schema, Name: table, Missing: true}
		}
		dict.Wanted[strings.ToLower(rel.Schema+"."+rel.Name)] = rel
	}
	return dict, nil
}

func flashbackLoadMySQLRelation(ctx context.Context, db *sql.DB, schema, table string) (*flashbackMySQLRel, error) {
	rel := &flashbackMySQLRel{Schema: schema, Name: table}
	rows, err := db.QueryContext(ctx, `
SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IFNULL(CHARACTER_SET_NAME, ''), COLUMN_KEY
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY ORDINAL_POSITION`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("information_schema.columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var col flashbackMySQLCol
		var key string
		if err := rows.Scan(&col.Name, &col.DataType, &col.ColumnType, &col.Charset, &key); err != nil {
			return nil, err
		}
		col.DataType = strings.ToLower(strings.TrimSpace(col.DataType))
		rel.Columns = append(rel.Columns, col)
		if strings.EqualFold(key, "PRI") {
			rel.PKCols = append(rel.PKCols, col.Name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rel.Columns) == 0 {
		return nil, fmt.Errorf("表 %s.%s 不存在", schema, table)
	}
	rel.NoPK = len(rel.PKCols) == 0
	return rel, nil
}
