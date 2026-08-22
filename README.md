# WorkBuddy Wild

**WorkBuddy/CodeBuddy + TraeWork 账号的 OpenAI 兼容代理 + 自动签到 Windows 托盘工具。**

双平台支持：聚合多个 WorkBuddy 和/或 TraeWork 账号，提供统一 OpenAI 兼容 API。
支持模型前缀区分（`workbuddy/<model>`、`traework/<model>`），自动路由到对应上游。

面向普通用户：**下载即用**，一个 exe 搞定账号登录、自动签到、积分查看、模型路由，为你的 AI 客户端（pi、Claude Code、Cursor 等）提供 OpenAI 兼容接口。

> 本项目源于两个原始项目，在其基础上重做为 Windows 双平台托盘应用：
> - [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — WorkBuddy/CodeBuddy 上游
> - [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api) — TraeWork SOLO 上游
> 
> 开发者/高级配置见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。

## 它解决什么问题

- WorkBuddy/CodeBuddy 和 TraeWork 账号的对话额度有限，多个账号可以共享/轮流使用
- 本项目把多个账号聚合成一个 **OpenAI 兼容 API**，自动按余额挑选账号、自动刷新 token、错误自动冷却
- 每天**自动签到**领额度，面板随时看积分、签到状态
- 全部在一个后台托盘程序里，无需任何编程环境

## 快速开始（普通用户）

1. **下载**：从 [Releases](https://github.com/rockswang/workbuddy-wild/releases) 下载最新版 `workbuddy-wild-windows-amd64.zip`，解压后双击 `workbuddy-wild.exe`
2. 稍等片刻会弹出提示框，显示 **OpenAI 兼容 API 地址**（默认 `http://127.0.0.1:7863`）——记住它，关掉即可
3. 右下角出现绿色 W 托盘图标，**单击 / 双击** 直接打开管理面板，**右击**弹出托盘菜单（打开面板 / 退出）
4. **添加账号**：面板点"＋ 添加账号"→ 选择 WorkBuddy 或 TraeWork → 自动打开无痕浏览器 → 登录对应平台 → 自动写入凭证并立即签到
5. **配置客户端**：把你的 AI 客户端指向
   ```
   Base URL: http://127.0.0.1:7863/v1
   API Key:  WorkBuddy2API   （面板"API-Key"处可随时修改）
   ```
6. 面板里还能：改自动签到时间（默认每天 09:00、21:00，支持分钟）、看签到状态、刷新积分、改监听地址、开机自启、查看日志、退出

> 首次运行自动生成 `config.json`（默认监听 `127.0.0.1:7863`）。程序常驻托盘，双击 exe 只是唤起/提示，主界面通过托盘图标打开。

## 与原始项目的区别

| | 原始 workbuddy2api | WorkBuddy Wild |
|---|---|---|
| 使用方式 | 命令行 + 脚本 + Docker，需安装 Go/Python/Docker | **单个 exe 双击即用**，零依赖 |
| 界面 | 无（纯 HTTP 服务） | **系统托盘管理面板**（添加账号/签到/积分/配置） |
| 登录 | Bash 脚本 + 手动复制粘贴授权 URL | 一键拉起**无痕浏览器**，自动完成登录轮询 |
| 签到 | 手动跑脚本 / 固定 9·21 点 | 面板**可视配置签到时间（支持分钟）**，即时生效 |
| 账号状态 | 无 | 面板实时展示**积分/签到记录/冷却状态**，单账号手动签到/刷新 |
| 监听配置 | 改配置文件重启 | 面板下拉选择 + **热切换**（不重启） |
| 日志 | 控制台 | 写 `data/app.log`，面板一键记事本打开 |
| 构建 | Linux 优先 | Windows 单文件，GitHub Actions 自动发布 |

## 功能清单

- ✅ OpenAI 兼容代理：`/v1/models`、`/v1/chat/completions`（流式/非流式）、`/status`、`/healthz`
- ✅ 双平台支持：同时接入 WorkBuddy（CodeBuddy）和 TraeWork 账号，模型 ID 以 `workbuddy/` 或 `traework/` 前缀路由
- ✅ 托盘面板：添加账号（无痕浏览器）、自动签到时间配置、签到状态、积分实时刷新
- ✅ API 监听主机/端口可改（`127.0.0.1` 本机 / `0.0.0.0` 全部 / 自定义），热切换
- ✅ 开机自启（注册表 Run 键）、查看日志、退出

## 系统要求

- Windows 10 21H2 及以上（自带 WebView2 运行时；更早版本首次运行自动安装）
- 无其他任何依赖

## 客户端兼容性（pi / Claude Code 等）

兼容标准 OpenAI 格式（`/v1/chat/completions`）。特别的兼容处理：

- **模型 ID 必须带平台前缀**：`workbuddy/<model>` 或 `traework/<model>`。
  例如 `workbuddy/hy3`、`workbuddy/deepseek-v4-pro`、`traework/DeepSeek-V4-Flash`。
  不带前缀的模型名会返回 400 错误。
- **`developer` 角色自动转换**：pi 等客户端对推理模型会用 `role=developer` 发送系统提示词。
  两条上游管道均已自动将 `developer` 归一化为 `system`，避免内容过滤误杀。

### 获取完整模型列表

所有可用模型通过标准 OpenAI `/v1/models` 端点获取，直接请求：

```bash
curl http://127.0.0.1:7863/v1/models -H "Authorization: Bearer WorkBuddy2API"
```

返回的列表中每模型都带 `workbuddy/` 或 `traework/` 前缀。
可以让 agent 先拉取模型列表，再依据下面的配置示例自动生成 models.json。

### pi 配置示例

编辑 `C:\Users\<用户名>\.pi\agent\models.json`，在 `providers` 中加入：

```jsonc
"workbuddy-wild": {
  "name": "WorkBuddy-Wild",
  "api": "openai-completions",
  "baseUrl": "http://127.0.0.1:7863/v1",
  "apiKey": "WorkBuddy2API",
  "models": [
    {
      "id": "workbuddy/hy3",
      "name": "hy3 (WorkBuddy)",
      "reasoning": true,
      "input": ["text"],
      "contextWindow": 192000,
      "maxTokens": 64000,
      "compat": { "maxTokensField": "max_tokens" }
    },
    {
      "id": "traework/DeepSeek-V4-Flash",
      "name": "DeepSeek V4 Flash (TraeWork)",
      "reasoning": true,
      "input": ["text"],
      "contextWindow": 1000000,
      "maxTokens": 50000,
      "compat": {
        "thinkingFormat": "deepseek",
        "supportsReasoningEffort": true,
        "maxTokensField": "max_tokens"
      }
    }
  ]
}
```

> 模型 ID 必须按 `workbuddy/` 或 `traework/` 前缀，否则请求被拒。
> 名称建议标注来源 `(WorkBuddy)` / `(TraeWork)` 以便区分同名模型。
> 建议只配置实际要用的模型，避免列表过长。

## 常见问题

**提示框显示的地址访问不了？**
检查是否改过监听地址：面板"API 监听"里看当前地址；或看 `data\app.log`（面板"查看日志"）。

**添加账号时浏览器没打开？**
面板登录框里有"复制链接"，可手动在浏览器（无痕窗口）打开完成登录。

**签到时间改了没生效？**
时间修改即写回 `config.json` 并立即生效，无需重启。

**Windows 提示“未知发布者”或报毒？**

本工具没有数字签名，因此可能会被 Windows 安全中心或杀毒软件拦截。如果报毒，请将本工具添加到排除项：

设置 → 更新和安全 → Windows 安全中心 → 打开 Windows 安全中心 → 病毒和威胁防护 → “病毒和威胁防护”设置/管理设置 → 排除项/添加或删除排除项 → 添加排除项（文件或文件夹），把 exe 或所在目录加进去。

如果对本仓库发布包不放心，可以自行基于源码构建（见开发文档）。

**TraeWork 多账号自动签到失败**

TraeWork有风控机制，无法确保多账号能可靠自动签到，这种情况建议使用客户端手动签到，登出客户端重新用另一账号登录即可签到。本工具的 API 转换功能不受影响。

## 开发者

构建方式、配置项、命令行工具、上游接口、已知限制等，见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。

## 免责声明

本项目为个人学习用途的第三方工具，与 WorkBuddy/CodeBuddy/TraeWork 官方无关；请遵守账号所在平台的服务条款。账号凭据仅保存在本机 `auths/` 目录。

## License

[MIT](LICENSE)（核心逻辑源于 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 与 [Sliverkiss/traework2api](https://github.com/Sliverkiss/traework2api)，同为 MIT）。
