# 数据库闪回

误 DELETE / UPDATE、误 DROP、误覆盖之后，DBA 需要的不是再连一次业务库「碰运气」，而是：**在指定时间窗内，把已经发生的变更还原成可审、可挑、可执行的 SQL**。

本项目是独立部署的数据库闪回服务。它只读 WAL / binlog / 离线数据副本，**不回写业务库**。产出 Undo（反向）或 Redo（原始）语句，由人确认后再落到目标环境。默认监听 `0.0.0.0:8620`，控制台：http://127.0.0.1:8620/

## 解决什么问题

线上误操作的窗口通常只有几分钟：行被删、字段被改、表被卸。传统手段要么整库回档（RTO 长、影响面大），要么靠备份点对点恢复（粒度粗、业务要停）。闪回把恢复对象收窄到 **一次误操作对应的事务时间窗 + 指定库/表**，先出 SQL，再决定怎么用。

适合：

- 误删、误改后，要按主键或整行把数据「变回去」
- 只想看这段时间发生了什么 DML / DDL，做审计或对账
- 云上 PostgreSQL 拿不到宿主机 `pg_wal`，但日志备份还在
- 实例已不可连，只剩本机 / 拷来的 PGDATA、WAL 目录

不适合：把本服务当成自动回滚机器人。它不提交工单、不在目标库执行生成的 SQL、同一时刻只跑一个闪回任务。

## 产品能力

| 场景 | 说明 |
| --- | --- |
| 自建 PostgreSQL 在线 | 连实例读 `pg_wal` / 归档，按 LSN 与事务 COMMIT 时间裁窗 |
| 腾讯云 PostgreSQL | 按时间窗拉取云上增量日志备份再解析（`vendor=tencent`，`cloud_instance_id=postgres-xxxx`） |
| PDU 离线 | 只读本机 PGDATA / WAL 副本：WAL 删除、WAL 更新前值、卸数、DROP TABLE 碎页扫描 |
| 自建 MySQL | 解析 row 格式 binlog，按时间窗与位点出 SQL |

范围：空表列表 = 整库，一行 = 单表，多行 = 多表。输出可选 Undo SQL 或原始 Redo SQL；PDU 还可导出 CSV。

控制台覆盖完整闭环：**选实例 → 填时间窗 → 预检查 → 确认执行 → 进度 / 日志 → 勾选 SQL 复制或下载**。实例地址、多云密钥与任务历史分开管理，任务元数据落在本项目自己的 Postgres，不占用业务库。

## 平台功能介绍

控制台无登录页，打开即用。侧栏五个入口，对应一次闪回从「登记地址」到「拿走 SQL」的工作台。

### 闪回任务

新建任务的主路径，三步：填写 → 预检查 → 执行确认。

- **在线闪回**：从「实例地址」选库，填数据库、目标表、起始/结束时间。快捷窗 5 分钟、10 分钟、1 小时、6 小时。输出 Undo SQL 或原始 Redo SQL，可勾选 insert / update / delete / ddl。
- **PDU 离线**：只读本机 PGDATA / WAL。场景包括 WAL 删除、WAL 更新前值、离线导出、DROP TABLE。表范围：空=整库，一行=单表，多行=多表。可选 SQL / CSV。
- PostgreSQL 页会提示提前设置 `REPLICA IDENTITY FULL`，避免 DELETE/UPDATE 在 WAL 里只有主键。
- 时间按北京时间解释，须盖住误操作的 COMMIT 时间。无时区字符串不会因服务进程 TZ 被当成 UTC。

### 预检查

执行前的门禁，不写业务库。在线路径核对 WAL/归档时间覆盖、CHECKPOINT 段、表是否存在、REPLICA IDENTITY、连续性等；腾讯云路径核验证书、Region、时间窗内 finished 增量包。覆盖文案统一用本地时区，不用回收段最早文件时间当起点。未通过不能进入执行。

### 任务详情

排队/执行中约 8 秒刷新进度和运行日志，完成后停止，避免整页抖动、冲掉已勾选 SQL。可分页查看生成语句，勾选后复制或下载 `.sql`；PDU 任务另有产物（CSV / DDL 等）下载。

### 历史记录

全部闪回任务列表。按状态、库名 / 实例 / 任务 ID 筛选，每页 10 条，带页码。任务连接来自地址库，不在这里改主机端口。

### 实例地址

目标库的唯一入口。任务页不手填 host/port。支持 PostgreSQL / MySQL，自建或腾讯云（补 vendor、云产品 ID、Region）。保存在本项目表，YAML 仅作兜底。

### 运维中心

多云密钥与下载参数（SecretId/SecretKey、默认 Region、拉取限速等）。保存到 `tbl_flashback_args` 立即生效，读取顺序：数据库 > 环境变量 > YAML。不要把密钥提交进仓库。

### 工具与集成

对已配置实例做连通自测：走真实预检查/解析链路，不提交工单、不在业务库落变更。用来验证账号、WAL/binlog 权限和云凭证是否齐。

## 使用方式

1. 在「实例地址」登记目标库（自建填 host/port；腾讯云补 vendor、云产品 ID、Region）。
2. 「闪回任务」选实例、库、表和时间窗。时间按北京时间理解，须盖住误操作的 COMMIT 时间。
3. 预检查过 WAL 覆盖、表是否存在、REPLICA IDENTITY 等后再执行。
4. 任务详情里查看进度与运行日志，勾选需要的 SQL 复制或下载。历史记录每页 10 条。

PostgreSQL 建议在误操作前对目标表执行 `ALTER TABLE … REPLICA IDENTITY FULL`。默认 IDENTITY 时 WAL 往往只记主键；闪回会尝试从堆页补齐其它列，行被 VACUUM 清掉后非主键列无法从 WAL 还原。

## 启动

1. 准备独立 Postgres，执行 [change/sql/tbl_flashback.sql](change/sql/tbl_flashback.sql)（进程不自动建表）。
2. 复制配置并改地址、账号（不要把云密钥提交进仓库）：

```bash
cp configs/config.example.yaml configs/config.yaml
```

3. 启动：

```bash
go run . svr -c configs/config.yaml
```

健康检查：`GET /healthz`。

## 接口

前缀 `/api/v1/flashback`：

- `POST /api/v1/flashback/tasks/precheck`
- `POST /api/v1/flashback/tasks`
- `GET /api/v1/flashback/tasks`
- `GET /api/v1/flashback/tasks/:id`
- `GET /api/v1/flashback/tasks/:id/sql`
- `GET /api/v1/flashback/tasks/:id/logs`
- `POST /api/v1/flashback/tasks/selftest`

- `GET/POST /api/v1/flashback/instances`、`PUT/DELETE /api/v1/flashback/instances/:id`（地址库）
- `GET/PUT /api/v1/flashback/cloud-settings`

## 配置要点

- `db`：闪回任务 / 日志 / SQL。
- `instances`：YAML 里的目标库；控制台「实例地址」优先。
- `flashback.args`：多云参数。控制台「运维中心」可编辑，保存到 `tbl_flashback_args`，立即生效。读取顺序：数据库 > 环境变量 > YAML。

## 腾讯云 PostgreSQL

1. 控制台「运维中心」填写腾讯云 `SecretId` / `SecretKey`，以及默认 Region（如 `ap-guangzhou`）。也可用环境变量 `FLASHBACK_TENCENT_SECRET_ID`、`FLASHBACK_TENCENT_SECRET_KEY`。
2. 「实例地址」新增目标库：`db_type=postgres`，`vendor=tencent`，`cloud_instance_id` 填云产品 ID（`postgres-xxxx`），`region` 与实例所在地域一致。
3. 云侧需已开启日志备份且保留覆盖任务时间窗。预检查会列举 finished 增量包，执行时按时间窗下载再解析。

YAML 示例见 `configs/config.example.yaml` 里的 `pg-tencent`。
