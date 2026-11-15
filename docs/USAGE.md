# 使用教程

从零到跑通，按顺序做就行。

---

## 一、刷入模块

1. 从 [Releases](https://github.com/Bxdj99/allin-go/releases) 下载最新的 `AllIn-Go-vX.X.X.zip`
2. 打开 **KernelSU / Magisk / APatch** 管理器 → 模块 → 从本地安装 → 选这个 zip
3. 重启手机

> 需要 root（KernelSU 或 Magisk）。没有 root 装不了。

---

## 二、拿到网关口令（第一次必做）

重启后，在模块管理器里找到 **All In**，点它的 **「运行」** 按钮。

终端会打印出这样一段：

```
================================================
 All In 网关已运行
================================================

网关口令（客户端 API Key 填这个）:

    mp-9948283d31207037930b83e3419400de

面板地址:  http://127.0.0.1:3425/
接口地址:  http://127.0.0.1:3425/v1
配置文件:  /data/local/tmp/allin-run/data/config.json

客户端这样填：
    Base URL : http://127.0.0.1:3425/v1
    API Key  : 上面那串口令
    模型名   : workbuddy/glm-5.3
================================================
```

**把那串 `mp-` 开头的抄下来**，后面客户端要填。

同时会自动打开面板，URL 里已经带上口令（`#key=...`），不用再手输。

### 如果没看到口令

三种可能：

| 情况 | 处理 |
|---|---|
| 点「运行」后终端只显示路径 | 用的是旧版模块，更新到 v1.4.3 以上 |
| 提示「还没读到网关口令」 | 首次启动要几秒生成，等 5 秒再点一次 |
| 想手动查 | 用 root 文件管理器看 `/data/adb/modules/allin/data/config.json`，找 `"authKey"` 字段 |

---

## 三、添加账号

打开面板（`http://127.0.0.1:3425/`），有三种方式：

### 方式 A：登录 WorkBuddy（推荐，免 Key）

1. 切到 **「账号」** 标签
2. 点 **「走 WorkBuddy 登录」**
3. 会用浏览器打开 WorkBuddy 登录页，**用手机号登录**
4. 登录完回到面板，会自动检测到并写入账号

登录成功后，这个号就能用 WorkBuddy 的所有模型（几十个，包括 glm-5.3、deepseek 系列）。

### 方式 B：其它供应商登录

| 供应商 | 按钮 | 流程 |
|---|---|---|
| Trae CN | 走 Trae 登录 | 浏览器授权 → 自动回填 |
| GitHub Copilot | 走 GitHub Copilot 登录 | 浏览器输入设备码 |
| Cline | 走 Cline 登录 | 浏览器输入设备码 |
| MiniMax / Qoder | 对应按钮 | 浏览器输入设备码 |
| CatPaw / Antigravity / Codex / 小浣熊 / 文心 | 对应按钮 | 链接授权 或 贴 Cookie |

### 方式 C：填 API Key

有官方 API Key 的（DeepSeek / OpenAI / Anthropic / OpenRouter / SiliconFlow 等）：

1. 切到 **「供应商」** 标签
2. 找到对应供应商卡片，点编辑
3. 填 `baseUrl` 和 `apiKey`
4. 点 **「同步上游模型目录」** 自动拉模型列表

---

## 四、客户端接入

任何支持 OpenAI 或 Anthropic 协议的客户端都能用：

- Cherry Studio / Chatbox / NextChat
- Claude Code / Cline / Continue
- 自建脚本

填这三项：

```
Base URL : http://127.0.0.1:3425/v1
API Key  : 第二步拿到的网关口令
模型名   : workbuddy/glm-5.3    ← provider/model 格式
```

### 常用客户端配置

**Cherry Studio**

```
设置 → 模型服务 → 添加 → 类型选 OpenAI
API 地址: http://127.0.0.1:3425
API 密钥: mp-xxxxx
```

**Chatbox**

```
设置 → 模型提供方 → 添加自定义提供方
API 域名: http://127.0.0.1:3425
API 路径: /v1
API 密钥: mp-xxxxx
```

**Claude Code（命令行）**

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:3425
export ANTHROPIC_API_KEY=mp-xxxxx
claude --model workbuddy/glm-5.3
```

> Claude Code 说的是 Anthropic 协议，网关会自动翻译成 WorkBuddy 的 OpenAI 协议。

---

## 五、面板功能速查

| 标签 | 能干什么 |
|---|---|
| **总览** | 运行状态、健康横幅（谁在冷却）、思考强度、WorkBuddy 签到、任务中心 |
| **供应商** | 增删改供应商、填 API Key、**同步上游模型目录**、展开看该供应商的账号 |
| **账号** | 全部账号一览（启停/删除）、手动加账号、各家登录按钮 |
| **用量** | 请求数、token 数、成功率、按供应商和模型分组、7 天趋势 |
| **设置** | 改网关口令、改监听地址、**系统提示词注入**、客户端 API Key 管理 |

### 几个值得用的

**思考强度**（总览页）

控制模型「想多久」。可以设全局默认，也可以在模型名后面加后缀临时覆盖：

```
workbuddy/glm-5.3:high    ← 高
workbuddy/glm-5.3:max     ← 拉满
workbuddy/glm-5.3:off     ← 关闭思考
```

**系统提示词注入**（设置页）

在这里写的提示词会**强制注入每个请求**，客户端自己带的 system 提示词压不住它。点「一键写入 M0SS 人设」可以载入预设。

**任务中心**（总览页）

一键跑 WorkBuddy 全套任务：活跃上报 → 签到 → 连登 → 领赠礼 → 补偿 → 补签卡 → 领养 → 出行 → 抽奖。每天 09/21 点也会自动跑。

**客户端 API Key**（设置页）

给不同设备各发一把 `sk-allin-xxx`，可以单独停用/删除。比共用一个网关口令好管理。

---

## 六、常见问题

### 面板显示「未填口令或口令不对」

回到第二步，点模块「运行」键拿口令，粘到面板顶部的输入框，点「保存并刷新」。

### 显示「有账号正在冷却」

账号被上游限流了。面板右上角横幅会显示**冷却到几点**。等它恢复，或者到「账号」页加更多号。

### 模型列表很少

点「供应商」页的 **「同步上游模型目录」**。WorkBuddy 正常能拉到 40+ 个模型。

### 回答到一半断了

v1.4.1 之前的版本有这个问题（连接层缺超时、流式无心跳）。更新到最新版。

### 网关没起来

```bash
# 看日志
cat /data/adb/modules/allin/gateway.log

# 手动启动
su -c "sh /data/adb/modules/allin/service.sh"

# 检查进程
su -c "ss -tln | grep 3425"
```

### 想让局域网其它设备用

面板「设置」里把监听地址从 `127.0.0.1` 改成 `0.0.0.0`，然后客户端填手机的局域网 IP：

```
http://192.168.x.x:3425/v1
```

⚠️ **注意**：局域网是明文 HTTP，口令和对话内容在网络上裸奔。只在自己信任的网络里这么干，别暴露到公网。

---

## 七、数据在哪

| 内容 | 路径 |
|---|---|
| 配置（含账号凭据） | `/data/adb/modules/allin/data/config.json` |
| 运行时配置 | `/data/local/tmp/allin-run/data/config.json` |
| 网关日志 | `/data/adb/modules/allin/gateway.log` |
| 运行日志 | `/data/local/tmp/allin-run/gateway.log` |
| 二进制 | `/data/adb/modules/allin/bin/allin` |

**`config.json` 里有账号凭据，不要发给别人。** 要分享账号池用面板的「导出账号」，导出的文件同样含凭据，自己把握。

---

## 八、更新

1. 从 Releases 下载新 zip
2. 模块管理器直接刷入覆盖
3. 重启

配置和账号都在 `data/` 里，升级不会丢。

> v1.4.2 之前有个 bug：刷了新模块但跑的还是旧二进制（`service.sh` 只在文件不存在时才复制）。如果你从更早的版本升级，建议升级后到面板「设置」里看一眼版本号对不对。
