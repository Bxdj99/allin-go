# All In（Go 版）

把 Android 手机变成随身 AI 网关：一个静态二进制，同时对外提供 **OpenAI** 与 **Anthropic** 两套兼容接口，对内把请求路由到二十多家上游供应商，并处理各家千奇百怪的协议差异。

作为 KernelSU 模块运行时：**不装 APK、不占桌面图标、全程非 root（uid 2000）**，常驻内存约 9 MB。

> **来源**：本项目是 [xunianooo/All-in](https://github.com/xunianooo/All-in)（作者 **Gaeul**，MIT）的 Go 语言重写版。
> 协议实现依据 [xunianooo/All-in](https://github.com/xunianooo/All-in) 与 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（作者 linguo2625469）翻译移植，
> 并参考 [yetone/magpie](https://github.com/yetone/magpie)。原始版权声明保留在 LICENSE 中。

## 为什么重写

原版 [xunianooo/All-in](https://github.com/xunianooo/All-in) 是 Java/Android 应用，功能齐但形态重：网关跑在 App 进程里，做成 KSU 模块就得「模块 + APK」两层，而且 `app_process` 方式常驻要 200 MB。这个版本用 Go 重写，编译成单个静态 ELF，模块直接执行。

另外补了两样原版没有的东西：**非 root 运行**（降权到 uid 2000）和 **WorkBuddy 任务中心全自动**。

## 特性

- **双协议入口**：`/v1/chat/completions`（OpenAI）、`/v1/messages`（Anthropic），两边可自动互转（含 `tools` / `tool_use` / `tool_result`）
- **账号池**：least-used 轮转、失败换号重试、分级冷却（401 / 429 / 5xx 各不同）
- **路由组**：`group/<id>` 按顺序挑目标
- **WorkBuddy 完整反代**：上游只吃流式 → 本地聚合给非流式客户端；工具名规范化、`tool_choice` 归一化、`reasoning_effort` 补齐
- **每日签到**：WorkBuddy 多账号 `daily-checkin`，09/21 点自动
- **登录接口**：Trae CN（浏览器授权）、GitHub Copilot（设备码）
- **Android 适配**：内置 CA 根证书（静态二进制在 Android 上找不到 `/etc/ssl/certs`）、DNS 兜底（Android 没有 `/etc/resolv.conf`）

## 支持的供应商

**完整反代（含各家协议差异处理）**

| 供应商 | 关键实现 |
|---|---|
| WorkBuddy / WorkBuddy AI | `/v2/chat/completions`、只吃流式、请求体洗刷 |
| Trae CN | 设备身份（EC P-256 + ECDSA 证明）、Windows 客户端伪装、SSE 事件流转换 |
| GitHub Copilot | 设备码 OAuth、`Copilot-Integration-Id` 等指纹头 |
| Codex | PKCE OAuth、chat → Responses 协议转换、`session-id` 粘性 |
| DeepSeek 网页版 | 23 轮 Keccak 变体 PoW、会话链管理、SSE patch 解析 |
| Qoder CN | COSY 信封签名（AES + RSA + 自定义 base64） |
| 小浣熊（商汤） | OAuth 授权码 + 一次性 refresh |
| Loomy（讯飞） | 手机验证码登录、双 token 头 |
| Cline | WorkOS 设备流 |
| MiniMax Code | PKCE 设备流（Anthropic 形态） |
| CatPaw（美团） | 三步会话机 `round → event → turn` |
| Antigravity（Google） | 自定义信封、强制 HTTP/1.1 |

**配置驱动（标准 OpenAI 兼容，填 key 即用）**

DeepSeek、OpenAI、Anthropic、OpenRouter、SiliconFlow、腾讯混元、智谱 GLM、AgentMore、Meoo、Runable、当贝、Tabbit 等。

**已知不完整**

- 文心一言网页版：只接了认证层，聊天需抓包取证后补 `internal/provider/wenxin/chat.go` 的 `Run`

## 用法

### 作为 KernelSU 模块

```bash
# 打包（需要 Go 1.24+）
go build -trimpath -ldflags "-s -w" -o allin .
mkdir -p module/bin && cp allin module/bin/
cd module && zip -r ../AllIn-Go.zip .
```

刷入模块后：`service.sh` 负责开机解包、降权到 `uid 2000` 启动、每 30 秒查端口（掉线自愈）、09/21 点签到。

管理面板：`http://127.0.0.1:3425/`（带 `#key=<authKey>` 自动登录）

### 直接跑

```bash
./allin -config ./config.json
# 首次启动会打印自动生成的网关口令
```

### 客户端接入

```
Base URL: http://127.0.0.1:3425/v1
API Key:  <面板里的 authKey>（不是供应商的 key）
模型名:   provider/model，例如 workbuddy/glm-5.3
```

## 配置

复制 `config.template.json` 改名 `config.json`，填上自己的凭据。最小示例：

```json
{
  "server": {"host": "127.0.0.1", "port": 3425, "authKey": ""},
  "providers": [
    {
      "id": "deepseek",
      "kind": "openai",
      "baseUrl": "https://api.deepseek.com/v1",
      "apiKey": "sk-xxx",
      "models": ["deepseek-chat"]
    }
  ]
}
```

`authKey` 留空会在首次启动时自动生成随机口令。

**⚠️ `config.json` 里是真实账号凭据，已在 `.gitignore` 排除，别提交。**

## 架构

```
main.go                    入口：读配置、装 DNS 兜底、起 HTTP
internal/config/           配置加载与落盘（结构兼容 Java 版）
internal/gateway/          HTTP 路由、鉴权、转发、各家 relay 分支
internal/bridge/           Anthropic ↔ OpenAI 协议互转
internal/provider/         供应商适配器
  ├── provider.go          统一请求构造 + 模型解析
  ├── pool.go              账号池（轮转 / 冷却）
  └── <vendor>/            各家协议实现
internal/netfix/           Android DNS 兜底
```

## 安全说明

- 网关默认**只监听 127.0.0.1**，不暴露到局域网
- 模块模式下进程跑在 **uid 2000（shell）**，不是 root；读不到 `/data/adb`，看不到模块痕迹
- 配置里的凭据以 `0600` 落盘
- 内置 CA 根证书仅用于 TLS 验证，不涉及证书绕过

## 许可

MIT
