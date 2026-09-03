-- PDU 离线闪回：任务引擎/附加参数，以及产物表。
-- 进程 EnsureSchema 也会 ADD COLUMN IF NOT EXISTS / CREATE IF NOT EXISTS。
ALTER TABLE tbl_flashback_tasks
    ADD COLUMN IF NOT EXISTS engine VARCHAR(16) NOT NULL DEFAULT 'native';
ALTER TABLE tbl_flashback_tasks
    ADD COLUMN IF NOT EXISTS extra TEXT NOT NULL DEFAULT '{}';

COMMENT ON COLUMN tbl_flashback_tasks.engine IS 'native=在线解析 pdu=本机离线 PDU 类能力';
COMMENT ON COLUMN tbl_flashback_tasks.extra IS 'PDU 场景 JSON：scene、路径、resmode、导出格式';

CREATE TABLE IF NOT EXISTS tbl_flashback_artifacts (
    id         BIGSERIAL PRIMARY KEY,
    task_id    VARCHAR(64)  NOT NULL,
    kind       VARCHAR(16)  NOT NULL DEFAULT '',
    relpath    TEXT         NOT NULL DEFAULT '',
    bytes      BIGINT       NOT NULL DEFAULT 0,
    row_count  INT          NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_flashback_artifacts_task ON tbl_flashback_artifacts (task_id, id);

COMMENT ON TABLE tbl_flashback_artifacts IS 'PDU 离线产物（CSV/DDL/COPY/SQL 文件）';
