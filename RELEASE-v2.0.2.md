# wild-work v2.0.2

- **修复长任务流式断流**：workbuddy / qoder 渠道的 SSE 请求在第 120 秒被
  `http.Client.Timeout` 硬性掐断（表现为「执行一会就停下来」且日志无任何记录）。
  现为两个渠道各加一个无超时的 `StreamHTTP`（与 traework 对齐），
  并让流中断时记录日志、计入账号冷却、补发 `data: [DONE]`。
  详见 `docs/2026-09-05-长任务流式断流修复.md`
- **WorkBuddy 限免模型接入 & 定价面板过滤修复**：`hy4-preview` / `hy3` 等
  `credits=x0.00` 的限免模型原本被误判为「非计费模型」而过滤掉。
  详见 `docs/2026-09-06-hy4限免模型与定价面板修复.md`
- 配置：`upstream.timeout_seconds` 默认 120 → 900（仅影响非流式请求）

实测：qoder 长任务 143.98s 正常完成（原 120.002s 硬断），
workbuddy 长任务 273.77s 正常完成（原 120s 必断）。
