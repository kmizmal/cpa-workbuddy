# 开发指南

## 工程命令

使用 Go **1.27.1**、Git 和可用的 C 编译器，启用 CGO。Windows 使用 MinGW GCC；Linux 使用 GCC；macOS 使用 Apple Clang。项目通过 `gcc` 命令调用独立 C 加载探针。

```sh
go run ./cmd/project bootstrap
go run ./cmd/project quality
go run ./cmd/project build
go run ./cmd/project test
go run ./cmd/project package
```

- `bootstrap`：安装固定版本的 gopls、golangci-lint，配置仓库的 `.githooks/commit-msg` 强制校验提交信息，并准备经过 SHA256 校验的打包许可素材。首次执行需要网络。
- `quality`：依次执行 tools、version、format、modules、vet、lsp、lint、build、test。检查不修改源码；warning、info、hint 同样阻断。未指定 `--message-file`、`--commit-range` 或 `--github-event` 时，还会校验当前分支相对上游尚未推送的提交信息。
- `build`：为当前系统和架构构建 CGO 动态库及生成头，输出到 `out/bin/`。
- `test`：构建动态库并运行 Go 行为测试、工程命令测试及实际 C ABI 加载测试。
- `package`：生成 `out/dist/cpa-workbuddy_<VERSION>_<goos>_<goarch>.zip` 和 `.zip.sha256`。发布时汇总五个平台的摘要为 `checksums.txt`。

定向检查与帮助：

```sh
go run ./cmd/project quality --stage lsp --stage lint
go run ./cmd/project --help
```

工程子进程的缓存、工具、临时文件和日志统一写入 `out/`。首次 `go run` 在工程入口启动前编译；需要隔离这一步时，先将 GOCACHE、GOMODCACHE、GOTMPDIR 指向 `out/cache/go-build`、`out/cache/go-mod`、`out/tmp/go` 的绝对路径，并创建这些目录。

## 代码布局与契约

| 位置 | 职责 |
| --- | --- |
| `cmd/cpa-workbuddy/` | CGO 动态库入口、原生导出与 C 缓冲区所有权 |
| `internal/plugin/` | CPA ABI / JSON wire、回调和分派 |
| `internal/workbuddy/` | 登录、账号身份、模型、刷新、Chat / Messages、协议转换、配额/签到、定时调度及控制面板 |
| `cmd/project/` | 构建、检查、打包和 Git 事件编排 |
| `VERSION`、`version.go` | 产品版本及嵌入入口 |

产品版本以根 `VERSION` 为唯一来源；原生 ABI 为 1，JSON schema 为 10，两者与产品版本独立。发布 tag 必须等于 `v` 加 `VERSION`。

插件由 CPA 加载，网络及凭证保存通过宿主契约。持久 provider 为 `workbuddy`，OAuth 入口为 `workbuddy` / `workbuddy-ai`；模型前缀默认为 `wb/` / `wb-ai/`（可通过 `cn_model_prefix` / `intl_model_prefix` 独立开关），AuthData.Prefix 留空。完整认证 ID 由站点、用户和企业作用域决定，可读文件名独立持久化；同身份重登从宿主记录复用名称。

CPA Manager 负责凭证保存、安装、刷新及一次 401 恢复；插件同时实现 `QuotaProvider`（`quota.*`）、受 Management Key 保护的插件控制面接口与静态资源面板 `/panel`（未鉴权资源页仅承载静态前端，所有业务接口均经 `mgmt.Middleware()` 鉴权），并在内部维护受 `Service.Close()` 生命周期管理的每日 ±3 小时浮动自动签到/成长点亮调度器。

## 测试与提交

优先运行覆盖改动的最窄检查，完成前运行一次完整 `quality`。CLI LSP 检查包含全部自有 Go 源码；新增测试关注消费者可见行为。验证使用命令行或隔离 headless，浏览器真实登录由用户操作。

首次执行 `go run ./cmd/project bootstrap` 会将仓库的 Git hooks 路径配置为 `.githooks`。之后每次 `git commit` 都会自动运行 `.githooks/commit-msg`；不符合提交格式时提交会被拒绝。CI 仍会在远端再次检查整个 PR 的提交历史。

```sh
go run ./cmd/project quality
go run ./cmd/project commit-msg --commit-range BASE..HEAD
```

质量门禁只检查，不修改源码，也不提交、推送或发布；提交前运行一次完整 `quality` 即可。推送到 GitHub 后由 Actions 在五个平台重新执行 lint、test 及本次推送的提交信息检查，作为发布的正式依据。

本仓库 XERJ 前缀为 `cpawb`。修改后执行：

```sh
xerj autoindex . --prefix cpawb --no-graph --code-analyzer code --yes
```

成功条件为 exit 0 或 3 且 `code_files_indexed > 0`。失败时移除同前缀并完整重建。非平凡改动在质量检查及索引同步后执行七维审查，结论放在交付回复；本机详细日志保留在忽略的 `out/`。

## CI 与发布

CI 在 Windows amd64、Linux amd64 / arm64、macOS amd64 / arm64 原生 runner 上运行 lint 和 test。每项核对实际 GOOS / GOARCH，并编译测试所需的临时库、执行真实 ABI 加载测试。PR 和 main 推送执行检查，也可手动运行 CI。

项目自有模块缓存目录保持可写，以支持各平台测试和临时工作树清理；Go 依赖清单仍按只读模式校验。检查工具由 `bootstrap` 按固定版本准备。

发布准备：

1. 更新根 `VERSION`，在 `.github/release-notes/` 编写对应版本的说明，文件名为 `v<VERSION>.md`，首行为 `# v<VERSION>`。
2. 完成代码、文档和验收收尾，运行本地质量检查。
3. 得到发布指令后，推送提交及与 VERSION 一致的 tag。
4. tag workflow 先运行五个平台的 lint 和 test，并按上一个 release tag 到本次 tag 的范围校验提交信息；全部成功后才启动五个平台的发布构建和打包；全部构建成功后才校验包及摘要，使用 Release notes 创建 GitHub Release 并上传产物。

发布工作流的远程执行结果以 GitHub Actions 为准。Release 发布前可在 Actions 检查各平台日志；失败任务必须修复后重新执行。CLI 先创建草稿并上传附件，全部上传成功后公开发布；上传失败遗留的草稿需确认后删除，再重跑发布任务。已发布的版本不覆盖。

包内包含动态库、生成头与 native.h、VERSION、README、开发指南、Release notes、项目及依赖许可、build-info.txt 与 checksums.sha256。打包采用白名单；凭证、账号数据、日志、缓存、测试参考目录均不进入产物。依赖许可按实际链接模块生成，补充素材由 bootstrap 下载并固定摘要校验。

## 参考与维护

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)：插件契约、凭证生命周期与 translator SDK。
- [CLIProxyAPI Management Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center)：官方管理页面。
- WorkBuddy 协议参考本地只读源码及测试；`_ref/` 为忽略目录，构建、测试、打包及 CI 均独立于该目录。

功能、安装和使用说明集中在 [README](../README.md)，首版验收结论保留在源码仓库的任务记录中。维护文档描述当前实现，详细探索与历史测试过程由本地日志保留。
