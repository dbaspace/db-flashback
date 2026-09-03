-- 已部署环境增量：闪回任务「获取日志 / 解析」两段进度。
-- Hub 进程不自动 ALTER。新环境直接执行 tbl_flashback.sql 即可（已含下列列）。
ALTER TABLE tbl_flashback_tasks ADD COLUMN IF NOT EXISTS log_total INT NOT NULL DEFAULT 0;
ALTER TABLE tbl_flashback_tasks ADD COLUMN IF NOT EXISTS log_done INT NOT NULL DEFAULT 0;
ALTER TABLE tbl_flashback_tasks ADD COLUMN IF NOT EXISTS parse_total INT NOT NULL DEFAULT 0;
ALTER TABLE tbl_flashback_tasks ADD COLUMN IF NOT EXISTS parse_done INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN tbl_flashback_tasks.log_total IS '获取日志总量（云增量包数 / 自建 WAL 文件数 / MySQL binlog 文件数）';
COMMENT ON COLUMN tbl_flashback_tasks.log_done IS '已下载（或已拉取）的日志份数';
COMMENT ON COLUMN tbl_flashback_tasks.parse_total IS '解析总量，通常与 log_total 相同';
COMMENT ON COLUMN tbl_flashback_tasks.parse_done IS '已解析份数';
