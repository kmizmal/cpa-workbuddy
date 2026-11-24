<div align="center">

# CPA WorkBuddy (`cpa-workbuddy`)

[![CI](https://github.com/ygq-future/cpa-workbuddy/actions/workflows/ci.yml/badge.svg)](https://github.com/ygq-future/cpa-workbuddy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ygq-future/cpa-workbuddy)](https://github.com/ygq-future/cpa-workbuddy/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**CLIProxyAPI 的 WorkBuddy 国内 / 国际双站点插件**

</div>

通过 CPA 管理页面登录 WorkBuddy，将账号可用模型接入 OpenAI、Responses、Claude 与 Gemini 客户端。插件 ID、动态库基础名和 CPA 配置键均为 `cpa-workbuddy`。

## 导航

- [功能概览](#功能概览)
- [安装与配置](#安装与配置)
- [登录与使用](#登录与使用)
- [Docker 部署](#docker-部署)
- [常见问题](#常见问题)
- [本地构建](#本地构建)
- [许可证](#许可证)

## 功能概览

- **国内与国际登录**：管理页面提供“WorkBuddy国内”和“WorkBuddy国际”两个浏览器登录入口。
- **多账号与企业身份**：国内、国际、个人及企业身份独立保存；同一身份重新登录更新原记录，保留宿主管理设置。
- **动态模型发现与可配置前缀**：按账号获取可用模型，默认国内使用 `wb/` 前缀、国际使用 `wb-ai/` 前缀；支持在配置或控制面板分别关闭，并在关闭后向后兼容带前缀的请求。
- **四种客户端协议**：支持 OpenAI Chat Completions、OpenAI Responses、Claude Messages 和 Gemini，提供流式与非流式响应。
- **工具、图片与推理**：支持工具调用闭环、图片输入和 reasoning / effort；具体能力取决于所选模型和账号权限。
- **凭证自动刷新与配额查看**：接入 CPA 的保存、刷新、请求恢复与 `QuotaProvider` 配额接口，支持个人套餐、活动奖励包与企业额度查询。
- **每日签到与自动调度**：支持国内账号每日签到（+100 积分）、国际账号成长中心热力图活跃点亮（+30/50 积分次日到账），以及围绕每日固定时间点 ±3 小时随机浮动的自动定时执行。
- **独立控制面板**：内嵌 `/v0/resource/plugins/cpa-workbuddy/panel` 控制面板，提供凭证状态、剩余积分明细、手动/批量签到与模型前缀开关配置。
- **账号导入导出**：控制面板勾选账号导出为 JSON，导入先展示脱敏预览再写入；同站点、同 UID 与企业作用域的记录按后出现者覆盖。
- **用量统计**：记录每次流式与非流式对话的 token 消耗，按本地模型倍率估算积分，并在面板汇总最近请求。

## 安装与配置

使用支持原生插件及 JSON schema 10 的官方 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)；推荐 CPA `v8.0.69` 或更新版本。管理页面使用官方 CPAMC。

### 通过插件商店安装

1. 重启 CPA 后在管理页面打开插件商店，搜索 **CPA WorkBuddy**。
2. 选择安装。商店按当前 CPA 的平台和架构选择发布包，核对发布摘要后将动态库写入 `plugins/<goos>/<goarch>/cpa-workbuddy-v<版本>.<扩展名>`，并把 `cpa-workbuddy` 写入配置启用。按页面提示重启 CPA 后生效。
3. 更新版本时在商店中重新安装即可。

商店通过 `api.github.com` 读取 Release 元数据，再从 GitHub Release 下载 ZIP 和 `checksums.txt`。CPA 的网络需允许这些请求及资产下载的重定向地址；网络受限时配置 CPA 代理，或改用手动安装。

### 手动安装

1. 从 [Releases](https://github.com/ygq-future/cpa-workbuddy/releases) 下载与 **CPA 运行平台和架构**一致的 `cpa-workbuddy_<版本>_<平台>_<架构>.zip` 及 `checksums.txt`。
2. 对照 `checksums.txt` 中对应文件的 SHA256 核对 ZIP，解压后将动态库放入 CPA 的插件目录，保留文件名。

| CPA 运行环境 | 发布包后缀 | 动态库安装路径示例 |
| --- | --- | --- |
| Windows x64 | `windows_amd64.zip` | `plugins/windows/amd64/cpa-workbuddy.dll` |
| Windows ARM64 | `windows_arm64.zip` | `plugins/windows/arm64/cpa-workbuddy.dll` |
| Linux x64 | `linux_amd64.zip` | `plugins/linux/amd64/cpa-workbuddy.so` |
| Linux ARM64 | `linux_arm64.zip` | `plugins/linux/arm64/cpa-workbuddy.so` |
| macOS Intel | `darwin_amd64.zip` | `plugins/darwin/amd64/cpa-workbuddy.dylib` |
| macOS Apple Silicon | `darwin_arm64.zip` | `plugins/darwin/arm64/cpa-workbuddy.dylib` |

### 启用插件

在 CPA 的配置文件中启用插件：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-workbuddy:
      enabled: true
      cn_model_prefix: true
      intl_model_prefix: true
      auto_checkin_enabled: true
      auto_checkin_time: "09:00"
```

- `cn_model_prefix` / `intl_model_prefix`：分别控制国内 `wb/` 与国际 `wb-ai/` 模型返回前缀（默认均为 `true`）。关闭某站点前缀后，模型列表直接返回原始模型 ID，且请求路由仍向后兼容带该站点前缀的模型名。若两侧同时关闭，同名模型会在国内与国际账号间共同调度。
- `auto_checkin_enabled` / `auto_checkin_time`：是否开启每日定时自动签到/成长点亮，以及每日基准时间（24 小时制 `HH:MM`，实际触发会在 `±3h` 内随机浮动）。
- **控制面板入口**：重启 CPA 后可通过管理页面插件菜单 **WorkBuddy** 或直接访问 `http://<CPA地址>/v0/resource/plugins/cpa-workbuddy/panel`。面板自动同步 CPA 主题并读取当前登录态的 Management Key（独立访问未登录时可在弹窗手动输入），支持查看凭证与剩余积分、执行手动/批量签到以及在线修改配置。
- **本地持久化**：面板保存的配置写入 `data/cpa-workbuddy/config.json`，重启后优先于 CPA 下发的 YAML；用量统计写入 `data/cpa-workbuddy/usage.json`，保留最近 7 天及最多 500 条请求记录。路径相对 CPA 工作目录，可用 `CPA_WORKBUDDY_CONFIG_PATH` 与 `CPA_WORKBUDDY_USAGE_PATH` 覆盖；删除对应文件即可恢复宿主配置或清空用量。

重启 CPA，在管理页面的插件列表确认 `cpa-workbuddy` 已加载并启用。
插件复用 CPA 的网络代理、凭证目录及管理鉴权。管理密钥与客户端 API key 分别由 CPA 配置。

## 登录与使用

1. 打开 CPA 管理页面，进入 OAuth 登录。
2. 选择 **WorkBuddy国内** 或 **WorkBuddy国际**，打开生成的登录链接。
3. 在浏览器完成登录、扫码及所需的身份授权，等待 CPA 提示成功。
4. 在认证文件列表确认账号，打开模型列表查看可用模型。
5. 将客户端的 API 地址和 API key 指向 CPA，选择带对应站点前缀的模型。

例如，通过 OpenAI Chat Completions 调用国际站模型：

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"wb-ai/hy4flash-free","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

请从账号实际模型列表选择模型。国内模型形如 `wb/<模型 ID>`，国际模型形如 `wb-ai/<模型 ID>`。登录会话有效期为五分钟，超时后重新发起即可。

## 账号导入与导出

重启 CPA 后，在管理页面打开 **WorkBuddy** 控制面板，在账号列表中勾选账号即可导出；导入时选择 JSON 文件，先查看脱敏预览，再确认要导入的记录。导入和导出都使用 UTF-8 JSON，不会修改 WorkBuddy 桌面端的认证文件。

文件可以是账号数组，也可以是参考实现使用的包装形式：`{"accounts":[...]}`、`{"data":[...]}` 或 `{"records":[...]}`。每条记录的核心字段如下：

| 字段 | 必需 | 说明 |
| --- | --- | --- |
| `access_token` | 是 | 访问令牌；导入时为空记录会跳过 |
| `refresh_token` | 否 | 刷新令牌 |
| `uid` | 是（当前插件） | WorkBuddy 账号 UID |
| `nickname` / `email` | 否 | 脱敏预览用的显示信息 |
| `domain` | 是（当前插件） | `copilot.tencent.com`、`workbuddy.cn`/`codebuddy.cn`（国内）或 `workbuddy.ai`/`codebuddy.ai`（国际）；缺省时回退到 `auth_raw.auth.domain` |
| `expiresAt` | 否 | epoch 毫秒；导出会保留毫秒精度 |
| `refreshExpiresAt` | 否 | epoch 毫秒 |
| `auth_raw` | 否 | 兼容参考实现的原始身份块，导入时只用于恢复企业身份等辅助字段 |

同一站点、同一 UID 和企业作用域的记录视为同一账号，批量文件中后出现的记录覆盖前一条；已存在账号也会覆盖其令牌但保留 CPA 的宿主管理元数据。空令牌、越界索引、错误 JSON、未知域名和属于另一站点的记录会计入“跳过”，不会写入凭据库。导入失败时错误提示只包含文件位置或字段原因，不显示令牌。

示例：

```json
[
  {
    "access_token": "<access-token>",
    "refresh_token": "<refresh-token>",
    "uid": "100000000001",
    "nickname": "账号A",
    "domain": "copilot.tencent.com",
    "expiresAt": 1760000000000
  }
]
```

导出响应只对明确勾选的账号生成，含令牌的响应使用 `Cache-Control: no-store`；请像密码备份一样保护导出文件，传输后及时删除，不要把它提交到 Git、日志或问题报告。导入不会主动刷新令牌，令牌失效仍需重新登录或等待现有刷新流程处理。

## Docker 部署

将插件目录挂载到容器，并确保配置中的 `plugins.dir` 指向容器内目录。例如，在现有 Compose 服务的 `volumes` 中加入：

```yaml
volumes:
  - ./plugins:/CLIProxyAPI/plugins:ro
```

Linux amd64 容器对应的宿主文件为 `./plugins/linux/amd64/cpa-workbuddy.so`。在 CPA 配置中填写 `dir: "/CLIProxyAPI/plugins"`，并启用上述两个开关；保留 CPA 的配置和凭证目录持久卷。

插件配置与用量文件默认写在容器内 CPA 工作目录的 `data/cpa-workbuddy/` 下；需要跨容器重建保留时，将该目录挂载到持久卷，或用 `CPA_WORKBUDDY_CONFIG_PATH` / `CPA_WORKBUDDY_USAGE_PATH` 指向已挂载路径。

容器内运行 CPA，浏览器登录在你自己的电脑上完成。选择与容器架构匹配的 Linux 包；Linux 发布包使用 glibc，Alpine / musl 环境需使用匹配环境构建的产物。

## 常见问题

**插件未加载？** 检查动态库的平台、架构、文件名和目录，确认 `plugins.enabled` 与 `plugins.configs.cpa-workbuddy.enabled` 均为 `true`，再查看 CPA 启动日志。Windows 如提示缺少 DLL，检查构建工具链所需的运行库。

**插件商店里看不到 `cpa-workbuddy`？** 确认 CPA 已更新到最新版本并重启，且能访问 GitHub API。若能看到插件但安装下载失败，检查代理是否允许 GitHub Release 资产及其重定向地址，或改用手动安装。

**看不到登录入口？** 确认插件已启用，更新官方 CPA 和管理页面后刷新。

**模型未出现或请求被拒绝？** 查看该账号的模型列表、禁用状态及模型排除规则；当开启前缀时使用包含 `wb/` 或 `wb-ai/` 的完整 ID，关闭对应站点前缀后可直接使用原始模型 ID 或继续使用带前缀 ID。模型和图片、工具、推理能力由上游账号权限决定。

**需要重新登录？** 在同一站点重新发起登录，同一身份会更新已有凭证。网络故障先检查 CPA 代理配置；不要在问题报告中附上 token、登录链接或完整凭证文件。

## 本地构建

准备 Go **1.27.1**、Git 和 C 编译器，在仓库根目录执行：

```sh
go run ./cmd/project bootstrap
go run ./cmd/project quality
go run ./cmd/project build
```

动态库位于 `out/bin/`；`go run ./cmd/project package` 生成当前平台 ZIP 和 SHA256。开发约定与发布流程见 [开发指南](docs/development.md)，版本变更见 [Release notes](.github/release-notes/v0.1.4.md)。

## 许可证

本项目采用 [MIT License](LICENSE)。二进制包中的 `licenses/` 目录包含实际链接依赖的许可声明。

