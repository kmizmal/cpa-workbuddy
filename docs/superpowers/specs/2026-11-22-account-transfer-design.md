# WorkBuddy 账号导入导出兼容设计

## 意图
为 `cpa-workbuddy` 增加与 `dsh-connect-workbuddy` 兼容的批量账号导入/导出，解决上游 Issue #2，同时保留 CPA 宿主负责的凭据持久化、刷新、调度和账号生命周期。

## 目标
- 接受参考项目的 JSON 数组格式，以及其明确支持的 `accounts` / `data` / `records` 包装。
- 导出 UTF-8 JSON，记录字段与参考实现一致：`access_token`、可选 `refresh_token`、`uid`、`nickname`、`domain`、`expiresAt`（毫秒）、`refreshExpiresAt`、`auth_raw`。
- 多账号导入逐条校验、按站点和稳定身份去重覆盖，返回导入/跳过计数；不让错误记录阻断有效记录。
- 不在预览、日志和错误消息中回显 token；导出只允许明确选中的账号，并通过管理接口返回 `Cache-Control: no-store`。
- 复用现有 `AuthData`、`credential`、`host.auth.save`/`host.auth.list` 契约，导入后走现有解析和存储流程。
- 覆盖合法数据、往返一致性、多账号、重复、跨站点/无效数据、缺字段、错误 JSON 与包装格式。
- 更新 README，说明入口、字段、示例、安全与限制。

## 非目标
- 不移植参考项目的独立 vault、隐藏账号列表、OAuth 扫码流程或 DSH UI。
- 不直接读写 WorkBuddy 桌面端认证文件；不改变现有刷新和调度行为。
- 不在导入时调用上游网络接口或刷新 token。

## 约束
- Go 现有架构与标准库优先；不新增运行时依赖。
- 账号 ID 必须继续由现有站点、UID、企业作用域规则生成，避免破坏已保存账号。
- 参考格式的 `expiresAt`/`refreshExpiresAt` 按 epoch milliseconds；当前内部 `time.Time`/JSON 表示在边界转换。
- 未识别域名必须跳过而不是默认归入国内站点；空 token、错误类型、非法字段不能进入宿主凭据存储。
- 导入写入使用现有 `host.auth.save`，重复账号覆盖旧凭据但保留宿主管理元数据。

## 方案比较
1. **宿主凭据适配层（采用）**：新增纯 Go transfer 编解码、校验、预览和合并逻辑，管理面板增加导入/导出端点，落库复用现有 CPA host auth 契约。改动集中、兼容现有运行链路，不引入第二套凭据数据库。
2. **完整移植参考 vault**：移植一账号一文件 vault、隐藏列表和 vault 级导入预览。与 CPA 的凭据所有权冲突，迁移和刷新双写风险高，不采用。
3. **只提供 CLI 文件转换**：实现格式转换但不接入管理面板。无法解决用户实际操作入口和 Issue #2 的可用性，不采用。

## 设计
### transfer 数据模型
新增 `transfer.go`：
- `TransferRecord` 对应参考项目 snake_case 字段；允许额外字段，保留 `auth_raw`。
- 解析接受裸数组和一层命名包装，顶层不是对象/数组或条目不是对象时返回带位置的错误。
- 必须有非空 `access_token`；`uid`、`domain`、过期时间等字段做类型和范围校验。
- `auth_raw` 仅用于保留原始身份信息，不作为 token 唯一来源；未知 `*_raw` 字段可丢弃以避免大文件膨胀。
- 稳定 ID 沿用现有 `credentialIdentity`（站点 + UID + enterprise scope），同一批次后者覆盖前者。

### 管理接口
在现有 `/v0/management/plugins/cpa-workbuddy` 下增加：
- `POST /transfer/export`：body `{ "auth_indexes": ["..."] }`，仅导出明确选中的当前站点账号；空选择或无匹配账号返回 400/409。
- `POST /transfer/preview`：body `{ "text": "..." }`，只返回账号索引、脱敏身份字段和 `has_token`。
- `POST /transfer/import`：body `{ "text": "...", "indexes": [0,1] }`，先完整解析，再逐条合并并通过 `host.auth.save` 写入；返回 `{ imported, skipped }`。
- 全部响应 `no-store`；token 仅存在于导出响应和导入请求内存，不进入日志和错误字符串。

### 面板
扩展嵌入式 `panel.html`：账号列表增加多选、导出按钮、文件选择、脱敏预览和导入确认；浏览器只保存待导入文本，不把 token 写入 localStorage。

### 兼容性
- 现有 `auth.parse`、`auth.refresh`、`model.for_auth`、`executor.execute` 不变。
- 导入生成的凭据使用 `credentialVersion=1` 与现有 JSON 字段；旧账号可照常读取。
- 对参考项目导出的 `expiresAt` 毫秒值保持毫秒精度，导出/导入往返不降级。

## 测试
新增 transfer 单元测试和管理路由测试，覆盖：合法导入、导出再导入一致性、多账号、重复覆盖、无 token、跨区域/未知域名、越界索引、缺字段、坏 JSON、数组包装及异常条目；面板只测关键请求/响应行为。

## 交付
运行 `go run ./cmd/project quality`、索引同步和项目要求的 post-impl/code-review；创建清晰提交历史，在目标仓库分支提交并创建 Pull Request，最终报告实现概要、文件、测试和 Issue #2 对应关系。

## 待实现决策
- 具体管理端点命名、面板布局细节和宿主保存请求体由实现者按现有 `panel.go`/`credentials.go` 契约落地。
- 若 CPA host 契约无法安全批量保存，则退化为逐条保存并在响应中报告失败条目，不改变现有运行流程。
