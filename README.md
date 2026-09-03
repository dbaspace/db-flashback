# db-flashback

独立运行的数据库闪回服务。从 `jupiter-db_manage_hub` 拷贝闪回解析能力，**不依赖 Hub 元库 / MDM / 工单**。

Hub 原项目功能不变，两边可同时运行。

## 和 Hub 的区别

| | Hub `:8600` | db-flashback `:8620` |
| --- | --- | --- |
| 实例解析 | `domain_instances` + MDM | `configs/config.yaml` 的 `instances[]` |
| 任务表 | Hub 元库 `tbl_flashback_*` | 本项目自己的 Postgres |
| 工单 | 可提交 DML | 不实现，自测跳过 |

请求里的 `instance_id` 必须等于配置中的 `instances[].id`。

## 启动

1. 准备独立 Postgres，人工执行 [change/sql/tbl_flashback.sql](change/sql/tbl_flashback.sql)（进程不自动建表）。
2. 复制配置并改地址、账号（不要把云密钥提交进仓库）：

```bash
cp configs/config.example.yaml configs/config.yaml
```

3. 启动：

```bash
go run . svr -c configs/config.yaml
```

默认监听 `0.0.0.0:8620`。健康检查：`GET /healthz`。

## 接口

路径与 Hub 对齐，前缀只有 `/api/v1/flashback`：

- `POST /api/v1/flashback/tasks/precheck`
- `POST /api/v1/flashback/tasks`
- `GET /api/v1/flashback/tasks`
- `GET /api/v1/flashback/tasks/:id`
- `GET /api/v1/flashback/tasks/:id/sql`
- `GET /api/v1/flashback/tasks/:id/logs`
- `POST /api/v1/flashback/tasks/selftest`（不提交工单）

- `GET/POST /api/v1/flashback/instances`、`PUT/DELETE /api/v1/flashback/instances/:id`（地址库）
- `GET/PUT /api/v1/flashback/cloud-settings`

不提供 `target-approvers`、`submit-dml-ticket`。

## 配置要点

- `db`：只存闪回任务 / 日志 / SQL。
- `instances`：目标库 host/port/user/password；云库可补 `vendor`、`cloud_instance_id`、`region`。
- `flashback.tencent_secret_id` / `tencent_secret_key`：也可用环境变量 `FLASHBACK_TENCENT_SECRET_ID`、`FLASHBACK_TENCENT_SECRET_KEY`。
- `flashback.args`：与 Hub `global_args` 同一套 key。控制台「多云」页可编辑，保存到 `tbl_flashback_args`，立即生效。读取顺序：数据库 > 环境变量 > YAML。
