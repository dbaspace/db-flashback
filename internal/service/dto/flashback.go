package dto

import (
	"time"

	"db-flashback/pkg/ginplus/response"
)

// FlashbackEnvelope /flashback/* 统一信封。
type FlashbackEnvelope struct {
	response.BaseResponse `json:",inline"`
	Result                any `json:"result"`
}

// FlashbackInstanceView 配置中的目标实例（不含密码明文）。
type FlashbackInstanceView struct {
	ID              string `json:"id"`
	DBType          string `json:"db_type"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	User            string `json:"user,omitempty"`
	Vendor          string `json:"vendor,omitempty"`
	CloudInstanceID string `json:"cloud_instance_id,omitempty"`
	Region          string `json:"region,omitempty"`
	Remark          string `json:"remark,omitempty"`
	SSHUser         string `json:"ssh_user,omitempty"`
	SSHPort         int    `json:"ssh_port,omitempty"`
	Source          string `json:"source,omitempty"` // db | yaml
	HasPassword     bool   `json:"has_password"`
}

// FlashbackInstanceSave 新建 / 更新实例地址。
type FlashbackInstanceSave struct {
	ID              string `json:"id" binding:"required"`
	DBType          string `json:"db_type"`
	Host            string `json:"host" binding:"required"`
	Port            int    `json:"port"`
	User            string `json:"user"`
	Password        string `json:"password"`
	SSLMode         string `json:"sslmode"`
	Vendor          string `json:"vendor"`
	CloudInstanceID string `json:"cloud_instance_id"`
	Region          string `json:"region"`
	Remark          string `json:"remark"`
	SSHUser         string `json:"ssh_user"`
	SSHPort         int    `json:"ssh_port"`
}

// FlashbackCloudVendorStatus 某云厂商密钥是否已配置。
type FlashbackCloudVendorStatus struct {
	Vendor     string `json:"vendor"`
	Name       string `json:"name"`
	IDKey      string `json:"id_key"`
	KeyKey     string `json:"key_key"`
	Configured bool   `json:"configured"`
}

// FlashbackArgItem 一条可编辑的多云参数（key 与 Hub global_args 一致）。
type FlashbackArgItem struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Secret      bool   `json:"secret"`
	Source      string `json:"source,omitempty"` // db / env / yaml
}

// FlashbackCloudSettings 独立项目的多云配置视图。
type FlashbackCloudSettings struct {
	Vendors           []FlashbackCloudVendorStatus `json:"vendors"`
	Args              []FlashbackArgItem           `json:"args"`
	TencentRegion     string                       `json:"tencent_region,omitempty"`
	OfflineAllowPaths []string                     `json:"offline_allow_paths,omitempty"`
	OfflineRoot       string                       `json:"offline_root,omitempty"`
}

// FlashbackCloudSettingsSave 保存多云参数。
type FlashbackCloudSettingsSave struct {
	Args []FlashbackArgItem `json:"args"`
}

// FlashbackTaskReq 预检查 / 创建任务共用入参。
type FlashbackTaskReq struct {
	InstanceID string   `json:"instance_id"` // 在线必填；PDU 离线可空
	Database   string   `json:"database" binding:"required"`
	Tables     []string `json:"tables,omitempty"` // 空/省略=整库；PG: schema.table；MySQL: db.table
	TargetTime string   `json:"target_time" binding:"required"`
	EndTime    string   `json:"end_time,omitempty"`
	StartXID   int64    `json:"start_xid,omitempty"`
	StopXID    int64    `json:"stop_xid,omitempty"`
	StartFile  string   `json:"start_file,omitempty"` // MySQL binlog 文件名，对齐 binlog2sql --start-file
	StartPos   uint32   `json:"start_pos,omitempty"`  // 默认 4
	StopFile   string   `json:"stop_file,omitempty"`
	StopPos    uint32   `json:"stop_pos,omitempty"`
	SQLType    string   `json:"sql_type,omitempty"`    // insert,update,delete,ddl 逗号分隔；空=全部
	OutputKind string   `json:"output_kind,omitempty"` // flashback|original，默认 flashback
	// DictTaskID 复用更早任务落盘的数据字典快照（对齐 WalMiner load_dictionary）。
	DictTaskID string `json:"dict_task_id,omitempty"`
	// CloudInstanceID 云产品实例 ID（如 postgres-xxxx）。MDM 已打 pg: 标签时可省略。
	CloudInstanceID string `json:"cloud_instance_id,omitempty"`
	// CloudRegion 腾讯云 API Region（如 ap-beijing）。MDM 已打 flash_region 时可省略。
	CloudRegion string `json:"cloud_region,omitempty"`

	Engine        string `json:"engine,omitempty"`    // native|pdu，默认 native
	PDUScene      string `json:"pdu_scene,omitempty"` // wal_delete|wal_update|unload|drop_table
	PGDataPath    string `json:"pgdata_path,omitempty"`
	ArchiveDest   string `json:"archive_dest,omitempty"`
	DiskPath      string `json:"disk_path,omitempty"`
	PGDataExclude string `json:"pgdata_exclude,omitempty"`
	StartWAL      string `json:"start_wal,omitempty"`
	EndWAL        string `json:"end_wal,omitempty"`
	PDUResMode    string `json:"pdu_resmode,omitempty"` // time|tx，默认 time
	ExportMode    string `json:"export_mode,omitempty"` // sql|csv|both
	IncludeDead   bool   `json:"include_dead,omitempty"`
}

// FlashbackCheckItem 单项预检查结果。
type FlashbackCheckItem struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Status  string `json:"status"` // passed / failed / warning
	Message string `json:"message"`
}

// FlashbackPrecheckResult 预检查输出。
type FlashbackPrecheckResult struct {
	OK            bool                 `json:"ok"`
	Items         []FlashbackCheckItem `json:"items"`
	Host          string               `json:"host,omitempty"`
	Port          int                  `json:"port,omitempty"`
	ServerVersion string               `json:"server_version,omitempty"`
	WALLevel      string               `json:"wal_level,omitempty"`
	ArchiveMode   string               `json:"archive_mode,omitempty"`
	WALFiles      int                  `json:"wal_files"`
	WALBytes      int64                `json:"wal_bytes"`
	WALFrom       *time.Time           `json:"wal_from,omitempty"`
	WALTo         *time.Time           `json:"wal_to,omitempty"`
	Covered       bool                 `json:"covered"`
	ParseMode     string               `json:"parse_mode,omitempty"` // online=BINLOG DUMP；file=自建拉 WAL；cloud=云增量包；pdu=本机离线
}

// FlashbackTask 任务详情。
// id 是任务 ULID，不是实例 id。
// instance_id / domain_instance_id 是 Hub domain_instances.id（≠ id）。
// mdm_instance_id 才是 MDM 资源 id（未纳管可空）。不要用任务 id 调 resource-db。
type FlashbackTask struct {
	ID               string                 `json:"id"`
	InstanceID       string                 `json:"instance_id"`
	DomainInstanceID string                 `json:"domain_instance_id,omitempty"`
	MDMInstanceID    string                 `json:"mdm_instance_id,omitempty"`
	Host             string                 `json:"host"`
	Port             int                    `json:"port"`
	Database         string                 `json:"database"`
	Tables           []string               `json:"tables"`
	TargetTime       time.Time              `json:"target_time"`
	EndTime          *time.Time             `json:"end_time,omitempty"`
	StartXID         int64                  `json:"start_xid,omitempty"`
	StopXID          int64                  `json:"stop_xid,omitempty"`
	StartFile        string                 `json:"start_file,omitempty"`
	StartPos         uint32                 `json:"start_pos,omitempty"`
	StopFile         string                 `json:"stop_file,omitempty"`
	StopPos          uint32                 `json:"stop_pos,omitempty"`
	SQLType          string                 `json:"sql_type,omitempty"`
	OutputKind       string                 `json:"output_kind"`
	Engine           string                 `json:"engine,omitempty"`
	PDUScene         string                 `json:"pdu_scene,omitempty"`
	PGDataPath       string                 `json:"pgdata_path,omitempty"`
	ArchiveDest      string                 `json:"archive_dest,omitempty"`
	DiskPath         string                 `json:"disk_path,omitempty"`
	ExportMode       string                 `json:"export_mode,omitempty"`
	Status           string                 `json:"status"`
	ErrorMessage     string                 `json:"error_message,omitempty"`
	Warning          string                 `json:"warning,omitempty"`
	WorkDir          string                 `json:"work_dir,omitempty"`
	WALBytes         int64                  `json:"wal_bytes"`
	WALFiles         int                    `json:"wal_files"`
	ChangeCount      int                    `json:"change_count"`
	Progress         *FlashbackTaskProgress `json:"progress,omitempty"`
	DMLTicketID      string                 `json:"dml_ticket_id,omitempty"`
	CreatedBy        string                 `json:"created_by,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	UpdatedAt        *time.Time             `json:"updated_at,omitempty"`
	StartedAt        *time.Time             `json:"started_at,omitempty"`
	FinishedAt       *time.Time             `json:"finished_at,omitempty"`
}

// FlashbackTaskProgress 获取日志 / 解析 两段进度（前端轮询 GET 详情绑 percent）。
// MySQL：unit=bytes，总量来自 SHOW BINARY LOGS，done 递增、remain 递减。
// PG：unit=files，按 WAL/增量包个数。
type FlashbackTaskProgress struct {
	Phase        string `json:"phase"`          // pending / fetch_logs / parse / catalog / wal_check / scan / restore / unload / dropscan / done / failed
	Unit         string `json:"unit,omitempty"` // bytes | files
	FetchDone    int    `json:"fetch_done"`
	FetchTotal   int    `json:"fetch_total"`
	FetchRemain  int    `json:"fetch_remain"`
	FetchPercent int    `json:"fetch_percent"`
	ParseDone    int    `json:"parse_done"`
	ParseTotal   int    `json:"parse_total"`
	ParseRemain  int    `json:"parse_remain"`
	ParsePercent int    `json:"parse_percent"`
}

// FlashbackTaskList 任务列表。
type FlashbackTaskList struct {
	Total int              `json:"total"`
	List  []*FlashbackTask `json:"list"`
}

// FlashbackSQLItem 一条生成的 SQL。
type FlashbackSQLItem struct {
	Seq        int        `json:"seq"`
	Kind       string     `json:"kind"`
	SchemaName string     `json:"schema_name"`
	TableName  string     `json:"table_name"`
	Op         string     `json:"op"`
	XID        int64      `json:"xid"`
	TS         *time.Time `json:"ts,omitempty"`
	Statement  string     `json:"statement"`
	Risk       string     `json:"risk,omitempty"`
}

// FlashbackArtifact 任务产物（CSV/DDL/COPY）。
type FlashbackArtifact struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"`
	RowCount int    `json:"row_count"`
}

// FlashbackPDUDiscoverReq 探测实例上的 PGDATA/WAL，或枚举本机 PGDATA 中的库/表。
type FlashbackPDUDiscoverReq struct {
	PGDataPath string `json:"pgdata_path,omitempty"`
	Database   string `json:"database,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
}

// FlashbackPDUDiscoverResult 离线目录探测结果。
type FlashbackPDUDiscoverResult struct {
	OK           bool                     `json:"ok"`
	PGVersion    string                   `json:"pg_version,omitempty"`
	PGDataPath   string                   `json:"pgdata_path,omitempty"`
	ArchiveDest  string                   `json:"archive_dest,omitempty"`
	RemotePGData string                   `json:"remote_pgdata,omitempty"`
	RemoteWAL    string                   `json:"remote_wal,omitempty"`
	OfflineRoot  string                   `json:"offline_root,omitempty"`
	Source       string                   `json:"source,omitempty"` // instance | staging | form
	LocalOK      bool                     `json:"local_ok,omitempty"`
	Databases    []FlashbackPDUDiscoverDB `json:"databases"`
	Message      string                   `json:"message,omitempty"`
}

// FlashbackPDUDiscoverDB 一个离线库。
type FlashbackPDUDiscoverDB struct {
	Name    string                       `json:"name"`
	OID     uint32                       `json:"oid"`
	Schemas []FlashbackPDUDiscoverSchema `json:"schemas,omitempty"`
}

// FlashbackPDUDiscoverSchema 一个 schema。
type FlashbackPDUDiscoverSchema struct {
	Name   string   `json:"name"`
	Tables []string `json:"tables,omitempty"`
}

// FlashbackSQLList SQL 预览。
type FlashbackSQLList struct {
	Total int                 `json:"total"`
	List  []*FlashbackSQLItem `json:"list"`
}

// FlashbackLogItem 任务日志。
type FlashbackLogItem struct {
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// FlashbackSelftestReq 在目标库自动建表并跑一轮 INSERT/UPDATE/DELETE 闪回。
type FlashbackSelftestReq struct {
	InstanceID string `json:"instance_id" binding:"required"`
	Database   string `json:"database" binding:"required"`
	// Reviewer 自测提交闪回工单的审核人，默认「系统」，须在目标库/域 approvers 中。
	Reviewer string `json:"reviewer,omitempty"`
	// OutputKind 为 original 时只做原始 SQL 输出，并按 insert/update/delete × 单表/多表/整库分别建任务；不提交工单、不跑 DDL。
	OutputKind string `json:"output_kind,omitempty"`
}

// FlashbackSelftestCheck 单项断言。
type FlashbackSelftestCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// FlashbackSelftestResult 自测报告。
type FlashbackSelftestResult struct {
	OK       bool                     `json:"ok"`
	Table    string                   `json:"table"`
	Tables   []string                 `json:"tables,omitempty"`
	TaskID   string                   `json:"task_id,omitempty"`
	TaskIDs  []string                 `json:"task_ids,omitempty"`
	TicketID string                   `json:"ticket_id,omitempty"`
	Checks   []FlashbackSelftestCheck `json:"checks"`
	UndoSQL  []string                 `json:"undo_sql,omitempty"`  // 闪回 SQL
	ParseSQL []string                 `json:"parse_sql,omitempty"` // 解析出的原始 SQL
	Warning  string                   `json:"warning,omitempty"`
}

// FlashbackSubmitDMLReq 把 undo SQL 提交为闪回工单（issue_type=flashback，审批仍走 DML 接口）。
// 审核人字段与 DML/DDL 同形，必须显式选择且在目标库/域 approvers 中，不自动指定。
type FlashbackSubmitDMLReq struct {
	AssignedTo       string `json:"assigned_to,omitempty"`
	AssignedToAlt    string `json:"AssignedTo,omitempty"`
	Reviewer         string `json:"reviewer,omitempty"`
	ReviewerPascal   string `json:"Reviewer,omitempty"`
	Approver         string `json:"approver,omitempty"`
	ReviewerUsername string `json:"reviewer_username,omitempty"`
	Description      string `json:"description,omitempty"`
}
