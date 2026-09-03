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
