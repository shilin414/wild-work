# wild-work 接入 CC Switch 指南

> 目标：把本地运行的 wild-work（多渠道账号聚合代理）作为新供应商，接入 CC Switch 一键切换使用。

## 0. 当前状态（已部署完成）

| 项目 | 值 |
|------|-----|
| wild-work 程序 | `C:\zcode\union2api\wild-work\bin\wild-work.exe`（v2.0.0） |
| 源码 | `C:\zcode\union2api\wild-work` |
| OpenAI 兼容 API | `http://127.0.0.1:7863/v1` |
| API Key | `WildWorkAPI` |
| Web 管理面板 | `http://127.0.0.1:7863/`（双击托盘 W 图标打开） |
| 运行状态 | 托盘常驻，PID 见任务管理器（wild-work.exe） |

**重要前提：** 先在 Web 面板点击「+ WorkBuddy / + TraeWork / + Qoder」添加至少一个账号（浏览器登录后自动完成）。没有账号时调用会报 `provider "xxx" has no account`。

---

## 1. 原理（为什么需要 CC Switch 参与）

```
Claude Code ──Anthropic协议──▶ CC Switch 本地代理(127.0.0.1:15721)
                                        │  协议转换 + 路由
                                        ▼
                              wild-work (127.0.0.1:7863) ──▶ 各渠道账号池
                                        OpenAI Chat Completions
```

- wild-work 只提供 **OpenAI Chat Completions** 端点（`/v1/chat/completions`、`/v1/models`），没有 Anthropic `/v1/messages`。
- Claude Code 原生走 **Anthropic Messages** 协议 → 必须经过 CC Switch 的**本地代理**做协议转换。
- Codex CLI 原生支持 OpenAI 格式 → 可直接接入，无需转换。

你的 CC Switch 已开启 `enableLocalProxy`（设置里的「本地路由」），Claude Code 当前 `ANTHROPIC_BASE_URL` 已指向 `http://127.0.0.1:15721`，条件已具备。

---

## 2. 方案一：Claude Code（推荐，协议自动转换）

1. 打开 CC Switch，左侧点击 **Claude Code** 分类（图标为 Claude）。
2. 右上角点击 **「添加供应商」**（+），选择 **「自定义 / Custom」**。
3. 填写供应商配置：

   | 字段 | 值 |
   |------|-----|
   | 供应商名称 | `wild-work`（可任意） |
   | API 格式 | **OpenAI（Chat Completions）**——若版本无此选项，选「OpenAI 兼容 / OpenAI Responses API」亦可，本地代理负责转换 |
   | 请求地址（Base URL） | `http://127.0.0.1:7863/v1` |
   | API Key | `WildWorkAPI` |
   | 完整 URL | 保持**关闭**（请求地址末尾不要加 `/`） |

4. **模型映射**：把 Claude Code 的默认模型槽位映射到 wild-work 模型（带渠道前缀）：

   | 槽位 | 建议模型 ID |
   |------|------------|
   | Sonnet | `workbuddy/auto` |
   | Opus | `workbuddy/glm-5.3`（或 `traework/DeepSeek-V4-Pro`） |
   | Haiku | `workbuddy/auto` |
   | Fable / Subagent | 按需映射 |

   > 完整模型列表：`curl -s -H "Authorization: Bearer WildWorkAPI" http://127.0.0.1:7863/v1/models`（添加账号后返回）。
   > 模型 ID 必须带渠道前缀：`workbuddy/`、`traework/`、`qoder/`。

5. 保存后，在供应商列表中点击该条目的 **「启用 / Activate」**。
6. 确认 CC Switch **设置 → 本地路由**：总开关开启，且 Claude 应用已勾选。服务地址应为 `http://127.0.0.1:15721`。
7. 新开一个终端，运行 `claude`，用 `/model` 可查看当前模型。切换供应商无需重启 CC Switch。

---

## 3. 方案二：Codex CLI（OpenAI 原生，无需转换）

1. 打开 CC Switch，左侧点击 **Codex** 分类。
2. 「添加供应商」→ 自定义，填写：

   | 字段 | 值 |
   |------|-----|
   | Provider Name | `wild-work` |
   | Base URL | `http://127.0.0.1:7863/v1` |
   | API Key | `WildWorkAPI` |
   | API 格式 | **Chat Completions**（wild-work 不支持 Responses API，勿选 responses） |
   | 默认模型 | `workbuddy/auto`（或其它带前缀模型） |

3. 保存 → 启用。CC Switch 会写入 `~/.codex/config.toml` 的 `[model_providers.wild-work]` 与 `auth.json`。
4. 新开终端运行 `codex` 验证。

---

## 4. 方案三：手动配置文件（不依赖 CC Switch 界面，兜底）

### Codex CLI（直接可用）

编辑 `~/.codex/config.toml`：

```toml
model = "workbuddy/auto"
model_provider = "wild-work"

[model_providers.wild-work]
name = "wild-work"
base_url = "http://127.0.0.1:7863/v1"
wire_api = "chat"
requires_openai_auth = true
```

并在环境变量（或 `auth.json` 的 tokens）中提供：

```
OPENAI_API_KEY=WildWorkAPI
```

### Claude Code

Claude Code 不能直连 OpenAI 端点，必须经协议转换层。建议就用 CC Switch 本地代理（方案一）；若想完全手动，可用 `musistudio/claude-code-router` 之类的转换工具指向 `http://127.0.0.1:7863/v1`，再在 `~/.claude/settings.json` 里配置 `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN`。

---

## 5. 常用模型 ID 参考（README 示例，添加账号后以 /v1/models 为准）

| 模型 ID | 说明 |
|---------|------|
| `workbuddy/auto` | WorkBuddy 自动路由 |
| `workbuddy/deepseek-v4-pro` | DeepSeek V4 Pro（WorkBuddy） |
| `workbuddy/glm-5.3` | GLM-5.3（WorkBuddy） |
| `traework/DeepSeek-V4-Pro` | DeepSeek V4 Pro（TraeWork） |
| `traework/glm-5.2` | GLM-5.2（TraeWork） |
| `qoder/qwen3.8-max` | Qwen3.8-Max（Qoder） |

## 6. 验证与排障

```bash
# 1. 服务是否在跑
netstat -ano | grep 7863

# 2. 模型列表（添加账号后应有数据）
curl -s -H "Authorization: Bearer WildWorkAPI" http://127.0.0.1:7863/v1/models

# 3. 对话测试
curl -s -X POST http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer WildWorkAPI" -H "Content-Type: application/json" \
  -d '{"model":"workbuddy/auto","messages":[{"role":"user","content":"hi"}]}'
```

- `provider "xxx" has no account` → 面板里还没添加该渠道账号。
- `invalid model` → 模型 ID 前缀或名称写错，先查 `/v1/models`。
- CC Switch 切换后不生效 → 新开终端（Claude Code 不热加载环境变量）。
- 换回原供应商：在 CC Switch 点击原供应商「启用」即可，官方登录/原账号配置不会被破坏（有原子写入 + 自动备份）。

## 7. 常用运维

- 打开面板：双击右下角托盘 W 图标。
- 添加账号：面板 → 渠道按钮 → 浏览器登录 → 自动写入 `bin/auths/`。
- 自动签到：默认每日 `09:00` / `21:00`，token 保活每日 `22:00`（配置在 `bin/config.json`）。
- 查看日志：托盘右键 → 查看日志。
