package flashback

import "time"

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	KindUndo = "undo"
	KindRedo = "redo"

	OutputFlashback = "flashback"
	OutputOriginal  = "original"

	EngineNative = "native"
	EnginePDU    = "pdu"
)

// TaskRow mirrors tbl_flashback_tasks.
type TaskRow struct {
	ID            string
	InstanceID    string
	MDMInstanceID string
	Host          string
	Port          int
	DatabaseName  string
	Tables        string
	TargetTime    time.Time
	EndTime       *time.Time
	StartXID      int64
	StopXID       int64
	StartFile     string
	StartPos      uint32
	StopFile      string
	StopPos       uint32
	SQLType       string
	OutputKind    string
	Engine        string
	Extra         string
	Status        string
	ErrorMessage  string
	Warning       string
	WorkDir       string
	WALBytes      int64
	WALFiles      int
	ChangeCount   int
	LogTotal      int
	LogDone       int
	ParseTotal    int
	ParseDone     int
	DMLTicketID   string
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     *time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// LogRow mirrors tbl_flashback_logs.
type LogRow struct {
	ID        int64
	TaskID    string
	Level     string
	Message   string
	CreatedAt time.Time
}

// SQLRow mirrors tbl_flashback_sqls.
type SQLRow struct {
	ID         int64
	TaskID     string
	Seq        int
	Kind       string
	SchemaName string
	TableName  string
	Op         string
	XID        int64
	TS         *time.Time
	Statement  string
	Risk       string
}

// ArtifactRow mirrors tbl_flashback_artifacts.
type ArtifactRow struct {
	ID        int64
	TaskID    string
	Kind      string
	RelPath   string
	Bytes     int64
	RowCount  int
	CreatedAt time.Time
}

// TaskListFilter list query.
type TaskListFilter struct {
	InstanceID string
	Status     string
	Keyword    string
	Offset     int
	Limit      int
}
