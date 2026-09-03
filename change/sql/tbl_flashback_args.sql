-- 独立项目多云参数（与 Hub global_args 同一套 key）。控制台编辑后落此表。
CREATE TABLE IF NOT EXISTS tbl_flashback_args (
    key         VARCHAR(255) PRIMARY KEY,
    value       TEXT         NOT NULL DEFAULT '',
    description TEXT         NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE tbl_flashback_args IS '闪回多云参数（与 Hub global_args 同一套 key），控制台可编辑';
COMMENT ON COLUMN tbl_flashback_args.key IS '如 flashback_tencent_secret_id';
COMMENT ON COLUMN tbl_flashback_args.value IS '参数值';
