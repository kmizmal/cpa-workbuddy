# WorkBuddy 账号导入导出 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task.

**Goal:** 在 CPA WorkBuddy 插件中实现与 `dsh-connect-workbuddy` 兼容的账号 JSON 导入/导出，解决 Issue #2。

**Architecture:** 新增纯 Go transfer 编解码/校验/合并模块；管理面板通过现有 `HandleManagement` 增加 preview/export/import 路由；导入使用现有 `AuthData`、`credentialIdentity` 与 `host.auth.save`，不引入第二套凭据存储。

**Tech Stack:** Go 标准库、现有 CPA host callback 契约、嵌入式 HTML/JavaScript 面板、项目现有测试编排。

**Spec:** `docs/superpowers/specs/2026-11-22-account-transfer-design.md`

## Global Constraints

- 参考文件是 JSON 数组或明确命名的 `accounts` / `data` / `records` 包装；记录字段使用 snake_case，过期时间 `expiresAt`/`refreshExpiresAt` 为 epoch milliseconds。
- 未识别域名、空 token、错误记录跳过；导入不把 token 写入日志、预览或错误文本。
- 重复账号按现有站点 + UID + enterprise scope 身份覆盖，宿主元数据保留。
- 不直接访问桌面端 auth 文件，不改变刷新、模型发现、调度链路，不新增运行时依赖。
- 根目录不生成构建产物；所有验证使用项目命令。

## Review Focus

- 同一文件内重复 UID/企业作用域：后者覆盖且只写一次；测试 `TestMergeTransferRecordsDeduplicatesByCredentialIdentity`。
- 未知/空域名不能默认进入国内站点；测试 `TestMergeTransferRecordsRejectsUnknownRegion`。
- `expiresAt` 毫秒往返不能被误当秒；测试 `TestTransferRecordRoundTripPreservesMillisecondExpiry`。
- 预览和错误不能泄露 token；测试 `TestPreviewTransferFileRedactsToken` 与错误字符串断言。
- 自有导出 `{accounts:[...]}` 必须能再次导入；测试 `TestParseTransferFileAcceptsAccountsEnvelope`。

---

### Task 1: Transfer 数据模型与纯逻辑

**Files:**
- Create: `internal/workbuddy/transfer.go`
- Test: `internal/workbuddy/transfer_test.go`

**Interfaces:**
- `type transferRecord struct`：`access_token`, optional `refresh_token`, `uid`, `nickname`, `domain`, `expiresAt`, `refreshExpiresAt`, `auth_raw`。
- `parseTransferFile(text []byte) ([]transferRecord, error)`。
- `previewTransferFile(text []byte) (transferPreview, error)`。
- `transferRecordOf(rec hostAuthRecord) (transferRecord, error)`。
- `credentialFromTransferRecord(record transferRecord, site site) (credential, error)`。
- `mergeTransferRecords(records []transferRecord, indexes []int, target site) (map[string]credential, imported, skipped int)`。

- [ ] 写失败测试：合法记录、`accounts`/`data`/`records` 包装、坏 JSON、非对象条目、缺 token、unknown region、重复覆盖、毫秒过期、脱敏预览。
- [ ] 运行 `go test ./internal/workbuddy -run 'Transfer'`，确认新接口/行为失败。
- [ ] 实现严格解析：顶层数组或一层命名包装；错误包含条目位置；只接受有限数值范围；空/未知域名跳过；按 `credentialIdentity` 去重。
- [ ] 实现导出投影：从 `hostAuthRecord.Stored` 生成参考 snake_case 记录，`auth_raw` 保存 `{auth, account}` 原始兼容块，时间转毫秒。
- [ ] 运行 transfer 测试并确认通过。

### Task 2: CPA 管理端点

**Files:**
- Modify: `internal/workbuddy/panel.go`
- Modify: `internal/workbuddy/host_client.go`
- Test: `internal/workbuddy/panel_test.go`

**Interfaces:**
- `POST /v0/management/plugins/cpa-workbuddy/transfer/preview` body `{text}`。
- `POST /v0/management/plugins/cpa-workbuddy/transfer/export` body `{auth_indexes:[]}`。
- `POST /v0/management/plugins/cpa-workbuddy/transfer/import` body `{text,indexes:[]}`。
- Responses: preview is token-free; export `{accounts:[...]}` with `Cache-Control: no-store`; import `{imported,skipped}`.

- [ ] 写路由失败测试：方法/JSON/空选择错误、preview 脱敏、export only selected accounts、import calls host save and reports skipped.
- [ ] 运行 `go test ./internal/workbuddy -run 'Management|TransferRoute'` 确认失败。
- [ ] 增加 `hostAuthSaveImported`：生成 version 1 browser credential，保留现有管理字段，使用 `host.auth.save`，每条独立保存并在失败时返回安全错误。
- [ ] 在 `HandleManagement` 中接入三个 POST 路由；导出先列出并读取所选 `auth_index`；导入先完整解析再写入，避免半解析半写入。
- [ ] 为 token 响应设置 no-store，错误仅包含位置/字段/宿主状态，不包含凭据值。
- [ ] 运行路由测试并确认通过。

### Task 3: 面板入口与文档

**Files:**
- Modify: `internal/workbuddy/assets/panel.html`
- Modify: `README.md`
- Test: `internal/workbuddy/panel_test.go` (关键 HTML/route smoke assertions)

- [ ] 增加账号多选、导出下载、文件选择、预览表和导入确认；待导入文本仅保存在页面内存，不写 localStorage。
- [ ] 明确展示成功/覆盖/跳过计数和字段错误，不显示 token 原文。
- [ ] README 增加操作入口、格式字段、JSON 示例、重复/无效/跨区域行为、UTF-8 与安全注意事项。
- [ ] 运行面板 smoke 测试并检查 HTML 不破坏现有 dashboard/settings 操作。

### Task 4: 全量验证与交付准备

**Files:**
- Modify: `.github/release-notes/v0.1.2.md` (若项目版本流程要求)
- Modify: `docs/development.md` (必要时补充测试说明)

- [ ] 运行 `go test ./...`。
- [ ] 运行 `go run ./cmd/project quality`，修复所有编译、静态检查和测试问题后重跑。
- [ ] 运行项目要求的 `xerj autoindex . --prefix cpawb --no-graph --code-analyzer code --yes`，确认 exit 0/3 且 `code_files_indexed > 0`。
- [ ] 检查 `git diff`，确认无 token、个人账号或临时文件；执行 post-impl/code-review。
- [ ] 按清晰提交历史提交到 feature 分支，准备向目标上游仓库发 Pull Request；记录本地测试、Issue #2 对应关系和未能自动推送/开 PR 的权限限制（如有）。
