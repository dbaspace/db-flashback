CREATE TABLE IF NOT EXISTS tbl_flashback_users (
    username   VARCHAR(64) PRIMARY KEY,
    password   TEXT         NOT NULL,
    perms      TEXT         NOT NULL DEFAULT '{}',
    enabled          BOOLEAN      NOT NULL DEFAULT TRUE,
    locked           BOOLEAN      NOT NULL DEFAULT FALSE,
    login_fail_count INTEGER      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tbl_flashback_sessions (
    token      VARCHAR(64) PRIMARY KEY,
    username   VARCHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_flashback_sessions_expires ON tbl_flashback_sessions (expires_at);
CREATE INDEX IF NOT EXISTS idx_flashback_sessions_user ON tbl_flashback_sessions (username);

COMMENT ON TABLE tbl_flashback_users IS '控制台登录账号，密码为 bcrypt 哈希；perms 为页面权限 JSON';
COMMENT ON TABLE tbl_flashback_sessions IS '控制台登录会话';
