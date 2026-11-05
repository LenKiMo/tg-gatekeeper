# MySQL 后端评估（评审 #18）

> 结论先说：**建议删掉 MySQL 后端**。它是"留着但没人验证"的状态——当前在 MySQL 8 上直接
> 跑不起来，且自动化覆盖为 0。若确有部署方在用，则按下面"保留"方案补齐集成测试，不接受
> 维持现状。
>
> 本文只做评估，不含代码改动；删除/保留由维护者拍板后另开 PR。

## 1. 现状

存储层是一套 SQL 同时跑 SQLite 与 MySQL，靠 `dialect` 字符串分支：

- `internal/store/sqlstore.go`（1013 行）：两套建表脚本（`schemas` map，SQLite 在 22-90、
  MySQL 在 92-130 附近），迁移逻辑 274-320，方言分支散落在 274、829、956 等处。
- `internal/store/factory.go`、`internal/provider/sql.go`：按 `group_store` /
  `session_registry` / `deletion_queue` / `provider.definitions.*.sql.driver` 分派 sqlite|mysql。
- `internal/config/config.go:284-286`、`293-300`：配置项与注释把 mysql 列为合法值。
- 依赖：`github.com/go-sql-driver/mysql v1.10.1`（直接依赖）+ `gorm.io/driver/mysql`（间接依赖，
  **代码里已无任何 gorm 使用**，是历史残留）。

## 2. 证据

1. **在 MySQL 8 上不可用**：`groups` 是 MySQL 8.0.2 起的保留字，建表语句加了反引号
   （`sqlstore.go:94`），但所有 DML 都是裸写：`sqlstore.go:279`（ALTER）、`371`/`400`/`429`
   （INSERT）、`407`/`420`（SELECT）、`442`（UPDATE）。也就是说：建表能过，第一次读写群配置就报
   语法错误——启动自检的"群配置读写可用"会直接失败。
2. **零自动化覆盖**：全仓库 `*_test.go` 里 `mysql` 出现 0 次。SQLite 路径有
   `sqlstore_test.go`（562 行）覆盖，MySQL 路径没有任何一条测试。
3. **维护成本是双向言**：每个新查询都要考虑两套类型系统（SQLite 动态类型 vs MySQL 严格类型）、
   两套 upsert 语法（`sqlstore.go:829` 有专门分支）、两套保留字。上面那个反引号 bug 正是这种
   分支遗漏的典型产物，而它已经存在了多个版本没被发现。

## 3. 三个选项

### A. 删除（推荐）

- 移除 `github.com/go-sql-driver/mysql`、`gorm.io/driver/mysql` 依赖；
- 删除 `schemas["mysql"]`、`openMySQL`、所有 `dialect == "mysql"` 分支（约 150-200 行）；
- 配置合法值收窄：`group_store` = `sqlite | yaml`，`session_registry` = `sqlite | memory`，
  `deletion_queue` = `sqlite | memory`，`provider.definitions.*.sql.driver` = `sqlite`；
- 同步 `configs/gatekeeper.example.yaml`、README（架构/部署段落里"SQLite 使用纯 Go 驱动"的说明可保留）。
- 风险：正在用 MySQL 的部署会启动失败（配置校验直接报错，而不是静默退化）。Release 说明里
  必须写明迁移路径（导出 → 用 SQLite 重建，或改用 YAML 群配置 + 内存会话）。
- 工作量：半天以内（含改校验与文档），无运行时风险。

### B. 保留并修复

- 所有语句统一给 `groups` 加反引号（SQLite 也接受反引号，可两边共用）；
- 加 MySQL 8 集成测试：CI 用 service container，`//go:build mysql` 隔离，不污染默认 `go test`
  （覆盖建表/迁移/群配置读写/CAS/会话 Claim/effect outbox）；
- 补 MySQL 的 upsert 与迁移分支的回归用例（至少 `ALTER TABLE` 幂等、重复列判断 `1060`）。
- 工作量：1-2 天（主要成本是 CI 与容器化的集成测试环境）。
- 风险：真正引入长期维护负担——每加一个查询都要跑两套集成测试。

### C. 维持现状（不推荐）

等于保留一个"看起来支持、实际不可用"的配置项。使用者踩到时得到的是启动失败 + 一段
SQL 语法错误，排查成本落在对方身上。

## 4. 需要维护者拍板的点

1. 是否确认**没有**正在运行的 MySQL 部署（删除前先搜一遍 agent-exchange / 各机的部署清单）。
2. 若删除：`storage.*` 的合法值收窄是否接受"老配置直接报错"的破坏性变更（建议接受，并写进
   CHANGELOG 的"行为反转"段）。
3. 若保留：CI 是否愿意承担 MySQL service container 的运行时间。
