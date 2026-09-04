-- 闪回任务 / 日志 / SQL。部署时执行 change/sql/ 下全部脚本。
CREATE TABLE IF NOT EXISTS tbl_flashback_tasks (
    id               VARCHAR(64)  PRIMARY KEY,
    instance_id      VARCHAR(64)  NOT NULL,
    mdm_instance_id  VARCHAR(128) NOT NULL DEFAULT '',
    host             VARCHAR(255) NOT NULL DEFAULT '',
    port             INT          NOT NULL DEFAULT 5432,
    database_name    VARCHAR(255) NOT NULL,
    tables           TEXT         NOT NULL DEFAULT '[]',
    target_time      TIMESTAMPTZ  NOT NULL,
    end_time         TIMESTAMPTZ,
    start_xid        BIGINT       NOT NULL DEFAULT 0,
    stop_xid         BIGINT       NOT NULL DEFAULT 0,
    start_file       VARCHAR(255) NOT NULL DEFAULT '',
    start_pos        BIGINT       NOT NULL DEFAULT 0,
    stop_file        VARCHAR(255) NOT NULL DEFAULT '',
    stop_pos         BIGINT       NOT NULL DEFAULT 0,
    sql_type         VARCHAR(64)  NOT NULL DEFAULT '',
    output_kind      VARCHAR(16)  NOT NULL DEFAULT 'flashback',
    status           VARCHAR(32)  NOT NULL DEFAULT 'pending',
    error_message    TEXT         NOT NULL DEFAULT '',
    warning          TEXT         NOT NULL DEFAULT '',
    work_dir         TEXT         NOT NULL DEFAULT '',
    wal_bytes        BIGINT       NOT NULL DEFAULT 0,
    wal_files        INT          NOT NULL DEFAULT 0,
    change_count     INT          NOT NULL DEFAULT 0,
    log_total        INT          NOT NULL DEFAULT 0,
    log_done         INT          NOT NULL DEFAULT 0,
    parse_total      INT          NOT NULL DEFAULT 0,
    parse_done       INT          NOT NULL DEFAULT 0,
    dml_ticket_id    VARCHAR(64)  NOT NULL DEFAULT '',
    created_by       VARCHAR(64)  NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ,
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_flashback_tasks_created_at ON tbl_flashback_tasks (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_flashback_tasks_instance ON tbl_flashback_tasks (instance_id);
CREATE INDEX IF NOT EXISTS idx_flashback_tasks_status ON tbl_flashback_tasks (status);

CREATE TABLE IF NOT EXISTS tbl_flashback_logs (
    id         BIGSERIAL PRIMARY KEY,
    task_id    VARCHAR(64) NOT NULL,
    level      VARCHAR(16) NOT NULL DEFAULT 'INFO',
    message    TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_flashback_logs_task ON tbl_flashback_logs (task_id, id);

CREATE TABLE IF NOT EXISTS tbl_flashback_sqls (
    id          BIGSERIAL PRIMARY KEY,
    task_id     VARCHAR(64)  NOT NULL,
    seq         INT          NOT NULL,
    kind        VARCHAR(16)  NOT NULL,
    schema_name VARCHAR(128) NOT NULL DEFAULT '',
    table_name  VARCHAR(128) NOT NULL DEFAULT '',
    op          VARCHAR(16)  NOT NULL DEFAULT '',
    xid         BIGINT       NOT NULL DEFAULT 0,
    ts          TIMESTAMPTZ,
    statement   TEXT         NOT NULL,
    risk        VARCHAR(255) NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_flashback_sqls_task ON tbl_flashback_sqls (task_id, seq);

COMMENT ON TABLE tbl_flashback_tasks IS 'PostgreSQL / MySQL 闪回任务（按时间点或 binlog file:pos / 库/表生成 undo SQL）';
COMMENT ON COLUMN tbl_flashback_tasks.output_kind IS 'flashback=undo SQL original=redo SQL';
COMMENT ON COLUMN tbl_flashback_tasks.status IS 'pending/running/succeeded/failed/cancelled';
COMMENT ON COLUMN tbl_flashback_tasks.start_file IS 'MySQL BINLOG DUMP 起始文件（对齐 binlog2sql --start-file）';
COMMENT ON COLUMN tbl_flashback_tasks.start_pos IS 'MySQL BINLOG DUMP 起始位点，0 表示默认 4';
COMMENT ON COLUMN tbl_flashback_tasks.stop_file IS 'MySQL BINLOG DUMP 结束文件（对齐 binlog2sql --stop-file）';
COMMENT ON COLUMN tbl_flashback_tasks.stop_pos IS 'MySQL BINLOG DUMP 结束位点，0 且指定了 stop_file 时表示扫完该文件';
COMMENT ON COLUMN tbl_flashback_tasks.log_total IS '获取日志总量（云增量包数 / 自建 WAL 文件数 / MySQL binlog 文件数）';
COMMENT ON COLUMN tbl_flashback_tasks.log_done IS '已下载（或已拉取）的日志份数';
COMMENT ON COLUMN tbl_flashback_tasks.parse_total IS '解析总量，通常与 log_total 相同';
COMMENT ON COLUMN tbl_flashback_tasks.parse_done IS '已解析份数';

CREATE TABLE IF NOT EXISTS tbl_flashback_args (
    key         VARCHAR(255) PRIMARY KEY,
    value       TEXT         NOT NULL DEFAULT '',
    description TEXT         NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE tbl_flashback_args IS '闪回多云参数（与 Hub global_args 同一套 key），控制台可编辑';
COMMENT ON COLUMN tbl_flashback_args.key IS '如 flashback_tencent_secret_id';
COMMENT ON COLUMN tbl_flashback_args.value IS '参数值';

CREATE TABLE IF NOT EXISTS tbl_flashback_instances (
    id                VARCHAR(64)  PRIMARY KEY,
    db_type           VARCHAR(32)  NOT NULL DEFAULT 'postgres',
    host              VARCHAR(255) NOT NULL,
    port              INT          NOT NULL DEFAULT 5432,
    db_user           VARCHAR(128) NOT NULL DEFAULT '',
    password          TEXT         NOT NULL DEFAULT '',
    sslmode           VARCHAR(32)  NOT NULL DEFAULT 'disable',
    vendor            VARCHAR(32)  NOT NULL DEFAULT '',
    cloud_instance_id VARCHAR(128) NOT NULL DEFAULT '',
    region            VARCHAR(64)  NOT NULL DEFAULT '',
    remark            VARCHAR(255) NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE tbl_flashback_instances IS '闪回实例地址，控制台维护；任务只引用 id';
