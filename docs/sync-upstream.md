# Fork 同步上游指南（追 rockswang/wild-work 更新）

> 本 fork：`shilin414/wild-work`，在原作者仓库基础上叠加本地运维补丁。
> 上游每次发版（如 v2.1.0 → v2.2.0），按本流程同步。

## 远程布局

| remote | 地址 | 用途 |
|---|---|---|
| `origin` | `https://github.com/shilin414/wild-work.git` | **自己的 fork**，日常推送目标 |
| `upstream` | `https://github.com/rockswang/wild-work.git` | 原作者仓库，只读，追更新用 |

## 标准同步流程

```bash
# 0. 确认工作区干净（有未提交改动先处理掉）
git status

# 1. 拉上游（本机 git 有引用写入缺陷，fetch 后必须复查！）
git fetch upstream
git for-each-ref refs/remotes/upstream/   # 确认 upstream/master 指向了新提交
# 若没更新（老毛病）：用 git ls-remote 拿真实 sha，手工写 loose ref：
#   SHA=$(git ls-remote upstream refs/heads/master | cut -f1)
#   mkdir -p .git/refs/remotes/upstream
#   printf '%s\n' "$SHA" > .git/refs/remotes/upstream/master

# 2. 合并
git merge upstream/master --no-edit

# 3. 解冲突（见下节「已知固定冲突」）

# 4. 验证（AGENTS.md 标准：build + vet + test 全绿）
go build ./... && go vet ./... && go test ./...

# 5. 构建本地运行产物（dist/ 已 gitignore，不入库）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work

# 6. 提交 merge、推送 fork
git commit   # merge 继续
git push origin master
# ⚠️ push 后同样复查 refs/remotes/origin/master，没写上就按上面办法手工补

# 7. 部署：dist/wild-work.exe → bin/wild-work.exe（需先停掉正在运行的服务！）
```

## 已知固定冲突与解法原则

本地补丁与上游长期重叠在 4 个文件，每次上游动这些区域大概率再现冲突。
解法总原则：**保留本地补丁语义，叠加（而非替换）上游新特性**。

| 文件 | 冲突原因 | 解法 |
|---|---|---|
| `internal/upstream/client.go` | 本地「限免模型 x0.00 保留」过滤 vs 上游定价字段演进 | 保留本地过滤条件（`credits==""` 才跳过），上游的 `parseBadge`/`Explicit`/`Color` 照单全收。两者互补：上游 `Free = IsExplicit && Rate==0` 只有在保留 x0.00 模型时才真正生效 |
| `internal/scheduler/scheduler.go` | 本地 `SkipCheckin` vs 上游 `ActivitiesOnly` | **两个字段并存**。语义不同：`SkipCheckin` 完全不跑签到；`ActivitiesOnly` 照跑 DailyCheckin 当保活但不上报。上游重构出的 `finishCheckin(uid, r)` 里记得补回本地加的 `Nickname` 字段 |
| `cmd/wild-work/main.go` | `qdSch` 行本地加了 `SkipCheckin: true` | 保留本地 `qdSch`，再整块叠加上游新增的渠道初始化（如 `wbaSch`） |
| `internal/app/app.go` | `Version` 常量 | **永远取上游的值**（如 2.1.0、2.2.0…），避免每次无谓冲突 |

机械化解冲突脚本（含上述规则的精确匹配）：本机 `union2api/tools/resolve-merge.py`
（在仓库外，不入库；冲突块结构变化后需同步更新脚本）。

## 红线（必读）

1. **绝不 `git add -A` / `git add .`**。`bin/` 是运行时目录（auths 凭证、config.json、
   exe、备份副本），`tools/` 是私用运维脚本——现已写入 `.gitignore`，但保持显式列文件的习惯，
   防止 gitignore 被上游合并意外回退时出事故。
2. 推送前可用 `git diff --cached --stat` 复查暂存清单，出现 `bin/`、`auths`、`*.bak` 立即停下。
3. `dist/wild-work.exe` 按 AGENTS.md 要求每次代码变更后本地重建，但 **不入版本控制**。
4. 本机 git 引用缺陷（fetch/push 报成功但 `refs/remotes/<name>/<branch>` 没写上）：
   一律 `git ls-remote` 取真实 sha 手工补 loose ref，不要相信 git 打印的成功信息。

## 本地补丁清单（相对上游的增量，2026-09-17 基线）

- `upstream/client.go`：StreamHTTP（SSE 长流不被 Timeout 掐断）+ 限免模型保留
- `upstream/sse.go`、`qoder/sse.go`、`traework/solosse.go`：SSE 异常结束补发 `[DONE]`
- `scheduler`：SkipCheckin / 停用账号也签到保活 / CheckinResult.Nickname / 失败统计日志
- `server/handler.go`：流式与聚合失败计入账号健康度并记日志
- `cmd/checkin-all/`：批量签到运维工具
- `docs/`：三篇修复记录 + 本指南

这些补丁中通用性强的（限免过滤、流式错误可观测）可考虑向上游提 PR，
被采纳后本地相应补丁即可删除，同步成本随之下降。
