# ARTEX 开发交接文档（换机迁移）

> 生成时间：2026-11-18 · 最近更新：2026-11-19（审查与修复之后）
> 工作区：`D:\code\ARTEX` · 基线 `main` @ `51291fe`
> **工作分支：`handoff/llm-user-agent`（已推送到 `xh1127/ARTEX`，当前提交见 `git log`）**
> 远端：`origin` → `https://ghfast.top/https://github.com/xh1127/ARTEX.git`（加速镜像，只用来拉取）
>       推送直接用地址：`git push https://github.com/xh1127/ARTEX.git handoff/llm-user-agent`

这份文档解决三件事：**代码在哪**、**新机器怎么起来**、**接下来该做什么**。

---

## 0. 一分钟清单

| 要做的事 | 命令 / 说明 |
|---|---|
| 拿到代码 | `git clone https://github.com/xh1127/ARTEX.git && cd ARTEX && git switch handoff/llm-user-agent`（见 §2-A） |
| **不要**带的东西 | `web/node_modules/`、`web/.next/`、`web/out/`、`data/`、`config.json`、`.env`、`jwt.key`、`artex` 二进制 |
| 新机器装什么 | Go **1.26.3+**、Node **20+**（本机实测 v24.19.0）、PostgreSQL **16**、git |
| 起来跑什么 | `cp config.example.json config.json` 填库 → `./dev.sh` |
| 验收跑什么 | `go build ./...`、`go test ./...`（需 `ARTEX_PG_DSN`）、`cd web && npx tsc --noEmit` |

---

## 1. 环境要求（务必对齐版本）

版本号取自 `go.mod` / `web/package.json` / `Dockerfile`，不是猜测：

| 组件 | 要求 | 出处 |
|---|---|---|
| Go | **1.26.3**（`go.mod` 的 `go 1.26.3`，低于此版本必须升级 toolchain） | `go.mod:3` |
| Node.js | **20+**；本机是 v24.19.0。`Dockerfile` 用 Node 20 是因为 Playwright 要求 ≥20 | `Dockerfile:16` |
| npm | 10+（本机 v12.0.2），需要 `npm ci` 装锁文件 | `web/package-lock.json` |
| PostgreSQL | **16**（`docker-compose.yml` 与 `install.sh` 都用 `postgres:16-alpine`） | `docker-compose.yml` |
| Git | 任意现代版本；注意行尾设置见 §6-A | — |

前端主要版本：Next **16.2.9**、React **19.2.7**、TypeScript **5.9.3**、Biome **2.5.1**。

> ⚠️ **源机器上找不到 `go` 可执行文件**（`PATH` 与 `/c/Program Files/Go`、`~/sdk`、`/c/Program\ Files\ (x86)` 都没有）。所以本次交接**没有跑过 `go build` / `go test` / `go vet`**。新机器上第一件事就是把这三个跑一遍，见 §4。

---

## 2. 代码怎么带过去

代码在 `xh1127/ARTEX` 的 **`handoff/llm-user-agent`** 分支上。不要再从旧工作区拷贝。

### 方案 A（推荐）：直接克隆分支

```bash
git clone https://github.com/xh1127/ARTEX.git
cd ARTEX
git switch handoff/llm-user-agent
```

访问 GitHub 慢时，可以用加速镜像克隆（`https://ghfast.top/https://github.com/xh1127/ARTEX.git`），但**镜像只能拉取**。推送要直连 GitHub：

```bash
git push https://github.com/xh1127/ARTEX.git handoff/llm-user-agent
```

> 推送用的 token 需要 **`repo` 和 `workflow`** 两个权限（分支里有对 `.github/workflows/` 的改动，缺 `workflow` 会被拒）。GitHub 不接受账号密码；在 Git Credential Manager 的登录框里，用户名填 GitHub 账号，密码处粘贴 token。

### 方案 B（备用）：用补丁 / bundle

仓库根可能还留着 `artex-wip.patch`（141 KB，28 个文件，含 9 个新增文件）。它现在**已被 `.gitignore` 的 `/*.patch` 覆盖**，不会被误提交。

```bash
# 补丁只能叠在基线提交上
git clone <仓库> && cd ARTEX
git checkout 51291fe            # 补丁基于这个提交，别叠到分支上
git apply artex-wip.patch
```

该补丁已在干净 `HEAD` 检出上实测过 `git apply --check` 通过、应用后 28 个文件全部落地。但**它不含后来那次提交里对 `.gitignore` 与 `HANDOFF.md` 的改动**，所以只在前述克隆方式不可用时才用它。

更稳的离线方式：源机器 `git bundle create ../artex-handoff.bundle handoff/llm-user-agent`，新机器 `git clone artex-handoff.bundle ARTEX`。

### 方案 C：整个目录拷贝

可以，但**必须排除**这些目录/文件（都是本机环境产物，跨机器无意义甚至有害）：

```
web/node_modules/     # 497 个包，平台相关的原生绑定，必须在新机器 npm ci 重装
web/.next/            # Next 构建缓存
web/out/              # 静态导出产物
web/tsconfig.tsbuildinfo
data/                 # 运行时数据目录（jwt.key、缓冲文件等）
config.json           # 含数据库密码（被 gitignore）
.env                  # 含 Postgres 密码（被 gitignore）
jwt.key               # JWT 签名密钥，绝不能跨环境复制
artex / artex.old / artex.new
```

`.git/` 只有 **8.7 MB**，带过去没问题，比补丁方案更完整（含历史与分支）。

---

## 3. 新机器上的初始化

### 3.1 数据库

```bash
# 最快的方式：单容器
docker run -d --name artex-pg -p 5432:5432 \
  -e POSTGRES_USER=artex -e POSTGRES_PASSWORD='换成你的密码' -e POSTGRES_DB=artex \
  -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
```

> 库和表**不需要手工建**：`db.Open` 会先连 `postgres` 库自动 `CREATE DATABASE`（`db/db.go:100` `ensureDatabase`），再幂等重跑 `db/schema.sql`（49 张表 + 全部 `ADD COLUMN IF NOT EXISTS`）。**重启即迁移**，这是本项目的既定机制。

### 3.2 配置

```bash
cp config.example.json config.json
# 编辑 config.json 的 database 段（host/port/user/password/dbname）
```

配置优先级（`config/config.go:131`）：`ARTEX_PG_DSN` 环境变量 **>** `config.json` 的 `database.dsn` **>** `config.json` 的 `database` 分字段。三者都没有则启动直接报错，**没有隐式默认值**。

> `config.example.json` 里写的端口是 **5433**，而 `install.sh`/`docker-compose.yml` 默认 **5432**。按你新机器实际映射的端口填，别照抄示例。

LLM 两种配法：环境变量（`ANTHROPIC_API_KEY` / `OPENAI_API_KEY`，可选 `ARTEX_LLM_PROVIDER` / `ARTEX_LLM_MODEL` / `ARTEX_LLM_BASE_URL` / `ARTEX_LLM_PROXY`），或在 UI 的「LLM 配置」页填。**UI 里配的会持久化进库并优先于环境变量**。

### 3.3 前端依赖与启动

```bash
cd web && npm ci && cd ..        # 必须用 npm ci，锁文件在 web/package-lock.json
./dev.sh
```

`dev.sh` 会同时起：后端 `go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788`、流量录制代理 `:8788`、前端 `next dev`。

首次打开 → **`/setup`** 设置管理员密码 → 登录。

### 3.4 单二进制模式（需要时）

```bash
cd web && npm run build:static && cd ..
rm -rf server/webui/dist && cp -r web/out server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
./start.sh
```

`-tags embedui` 是开关，见 `server/webui_embed.go`（有 tag）与 `server/webui_stub.go`（无 tag 时返回 404 提示「前端未内嵌」）。不带 tag 编译出来的是纯后端，前端只能走 `npm run dev`。

---

## 4. 验收（新机器上先跑这一遍）

```bash
# 1) 编译
go build ./...

# 2) 静态检查
go vet ./...

# 3) 单元测试
#    注意：不设 ARTEX_PG_DSN 时，所有需要数据库的包会 t.Skip，看起来"全绿"其实没测
export ARTEX_PG_DSN='postgres://artex:密码@127.0.0.1:5432/artex?sslmode=disable'
go test ./...

# 4) 前端类型检查与 lint
cd web
npx tsc --noEmit
npm run check          # biome check
```

测试基建有两个关键点，不知道会踩坑：

- **需要独立测试库**：`db`、`agent`、`server`、`evidence` 四个包都有 `TestMain`，会在 `ARTEX_PG_DSN` 指向的库上取 **PostgreSQL 咨询锁 7337741002** 来串行化跨包清理（`server/testmain_test.go`、`db/testmain_test.go` 等）。所以**别指向生产库**，测试会 `DELETE` 数据。
- **静默跳过**：`requestHeadersDatabase()`（`server/llm_request_headers_test.go:14`）在 DSN 为空时 `t.Skip`。新增的 9 个测试文件几乎全靠它，因此**没设 DSN 就等于没测**。

> CI 现状：`.github/workflows/test.yml` 在推送到 `main` / `handoff/**` 和 PR 时运行——`go vet`、带 `-tags embedui` 的编译、`go test`（带 `postgres:16` service，**阻断**）、前端 `tsc`（阻断）与 `biome check`（暂不阻断，存量问题未清）。`release.yml`（打 tag 构建五平台）与 `manual-build.yml`（手工构建 Windows amd64）本身不跑测试。
>
> 没有配置数据库时，依赖数据库的测试会**跳过**（不再报失败）；所以本地全绿不代表这些测试跑过，以 CI 为准。

---

## 5. 分支 `handoff/llm-user-agent` 里有什么

提交 `608bb58`，30 个文件，+2536 / −370。一条主线加四块顺带加固：

### 5.1 主线：LLM 配置支持自定义 User-Agent

| 层 | 文件 | 做了什么 |
|---|---|---|
| DB | `db/schema.sql`、`db/config.go` | `llm_profiles` 加 `user_agent TEXT NOT NULL DEFAULT ''`（建表 + `ADD COLUMN IF NOT EXISTS` 双写）；读列、INSERT、两条 UPDATE 全部带上 |
| Agent | `agent/provider.go` | `Config.UserAgent`；`NormalizeUserAgent()`（≤512 字节、仅可打印 ASCII 32–126，先校验再 trim，防 CRLF 注入）；`quotaAwareTransport` 在 `RoundTrip` 里**先 `req.Clone()` 再改头** |
| Agent | `agent/websearch.go`（新增） | DeepSeek 搜索后端自建 runner，带上 profile 的 UA |
| Server | `server/server.go`、`server/server_mgmt.go`、`server/dto.go`、`server/manager.go` | `testLLM` / `pgListModels` / `pgSaveProfile` 支持 UA；`session_header_key`、`max_tokens_field`、`user_agent` 三个字段统一成 `*string` 语义（**省略=用已存值，空串=显式清除**） |
| Web | `web/src/app/(main)/system/llm/page.tsx`、`lib/api.ts`、`lib/types.ts` | UA 输入框 + 三个预设（Claude Code / Codex CLI / OpenCode，固定版本示例，可编辑） |

### 5.2 顺带加固（价值不亚于主线）

1. **`quotaAwareTransport` 不再改调用方请求**——原来直接改 `req.Header`，共享 provider 下是数据竞争。
2. **provider 热重载加 generation 校验**（`server/server.go:419` `applyLLM`）：`profCacheGen` + `llmApplyMu` + `provCacheMu`，堵住「构建中的旧 provider 回填缓存」。`invalidateProfileAgents` 现在把两个缓存放在同一代里清。
3. **settings 写入事务化**：新增 `DB.SetSettings`（`db/settings.go:34`，单事务 + 稳定 key 排序防锁序死锁）；`putSettings`（`server/server.go:3424`）改成「先全量校验 → 一次事务落库 → 最后发布内存态」。
4. **配置先校验后落库**：`pgActivateProfile` / `pgSaveProfile` 在改库前先 `cfg.NewProvider()` 试构建，非法配置不会污染激活态。

### 5.3 新增测试文件（9 个，约 1.4k 行）

```
agent/user_agent_test.go            agent/websearch_test.go
db/config_user_agent_test.go        server/chat_profile_cache_test.go
server/llm_request_headers_test.go  server/llm_runtime_config_test.go
server/llm_test_token_field_test.go server/llm_user_agent_test.go
```

---

## 6. 已知问题与待办

2026-11-19 做过一轮全项目审查与修复，详见 `CHANGELOG.md` 的 `[Unreleased]`。本节原先列出的条目（搜索凭据用错配置、CI 不跑测试、已落库却回 500、`web_search` 无 panic 兜底、重试无上限、死 setter、构建失败原因不可见等）都已处理。

### 升级后行为有变化的地方

`CHANGELOG.md` 的「安全加固 → 行为变化」一节是完整清单，最容易踩到的三条：

1. 首次打开 `/setup` 需要填**初始化令牌**（启动日志，或程序目录下的 `setup-token` 文件）。
2. 改了 LLM 的 Base URL、代理地址、MCP 的 URL 之后，对应的 Key / 密码 / 请求头要**重新填写**。
3. 推送不再读取 `HTTP(S)_PROXY`；需要代理的话在「推送」页填**推送代理**。

### 仍未处理（有意为之，改之前先想清楚）

- **更新包没有签名**：`SHA256SUMS` 与安装包来自同一个 Release，只防损坏不防篡改。要做签名得先决定密钥由谁保管。
- **一键更新的仓库地址写死为上游 `Autumn-27/artex`**（`selfupdate/github.go`）。本 fork 用 `manual-build.yml` 且**版本号留空**出包（`v0.0.0-manual.*`，一键更新自动禁用）。要走自己的版本线，先把 `Repo` 改成 fork，并处理 `release.yml` 里推送到 `autumn27/artex` 的 Docker 步骤。
- **实时推送的 token 在 URL 里**（EventSource 不能带请求头）。README 的反代一节有访问日志的建议；彻底解决需要改成一次性的连接票据，并处理四处 EventSource 的重连逻辑。
- **LLM / MCP 地址没有 SSRF 限制**：加了会挡住本机的 ollama 等合法用法。
- **自定义工具（HTTP 类型）的请求头与代理仍原样回显**：与 MCP 是同一类问题，尚未打码。
- **目标拆解到一半被打断**时可能只留下部分目标，任务会带着不完整的目标继续跑。
- `server/server.go` 仍然很大；`agentsForTask` 在查库与构建 provider 期间持有全局锁，任务很多时会互相等待。
- 前端 `biome check` 有约 135 个存量错误，CI 里这一步暂不阻断。

### 还没审查过的部分

前端的大部分页面与组件、`db` 层的大部分、`report` / `enrich` / `guard` / `intercept` / `sidequestion`、skills、任务归档与证据导出、`start.bat`。

---

## 7. 目录地图（按职责，不是按体积）

```
cmd/artex/          入口；-addr / -data / -proxy 三个 flag；自更新 bootstrap 在最前
config/             config.json 加载与 DSN 解析（env > config.json）
db/                 PostgreSQL：schema.sql(1389 行/49 表) + 全部数据访问 + seed
server/             HTTP API(55 文件 ~25k 行)：任务引擎、调度、LLM 装配、settings
  ├ server.go         路由 + LLM 热重载 + settings（已偏大）
  ├ server_mgmt.go    管理端 CRUD（profile / agent / 工具）
  ├ manager.go        Manager：任务、流量、switches
  ├ task_llm.go       每个任务的 agent bundle（planner/worker/main/goals）
  ├ assembly.go       工具装配：ToolAugment → ToolResolve → panic 兜底
  └ llmpool.go        故障转移链
agent/              planner / worker / mainagent / chat 四类 agent + provider 包装
traffic/            MITM 录制代理（go-mitmproxy）
guard/ interrupt/   命令拦截与人在环路审批
llmrec/ llmpool/    请求录制、故障转移
notify/             6 种 IM 推送渠道
sidequestion/       旁路提问
selfupdate/         一键更新与回滚
skills/             3 个内置 skill（api-recon / playwright-cli / scopesentry）
web/                Next.js 16 前端（210 个受版本控制的文件）
```

**双图架构**、worker 间信息交换、planner 多轮共享 todo 的设计说明在 `README.md` 的「系统技术架构」一节（含 mermaid 图），不在本文重复。

---

## 8. 环境坑位清单

### A. 行尾（Windows ↔ Linux 之间最容易炸）

```
.gitattributes:   *.bat text eol=crlf    *.sh  text eol=lf
源机器 core.autocrlf = true
```

`.sh` 必须保持 LF，否则在新机器（尤其容器或 WSL 里）执行会报 `bad interpreter: /bin/sh^M`。如果新机器也是 Windows 且 `autocrlf=true`，克隆后检查一下 `start.sh` / `dev.sh` 的行尾。

git 会大量提示 `LF will be replaced by CRLF the next time Git touches it`——这是正常的，不是错误。

### B. 前端 dev 端口

`web/package.json` 的 `dev` 脚本固定为 `next dev -p 5173`，与 `dev.sh`、README 一致：开发时访问 **http://localhost:5173**。

### C. 端口占用

`:8787`（后端+前端 API）、`:8788`（录制代理，仅监听 127.0.0.1）、`:5432`（Postgres）。`docker-compose.bench.yml` 会再占一套，基准测试前先停掉常规服务。

### D. 代理相关（四套，别混）

| 配置项 | 作用范围 |
|---|---|
| 每个 LLM profile 的 `proxy` | 只影响该 profile 的模型请求 |
| `global_proxy`（系统设置） | 所有**目标**流量；录制开启时成为 MITM 的上游 |
| `web_search_proxy`（系统设置） | 只影响搜索后端出口 |
| `notify_proxy`（推送页 · 全局设置） | 只影响 Webhook 类推送渠道；邮件始终直连 |

留空 = 直连。四者都**不回退** `HTTP_PROXY`/`HTTPS_PROXY` 环境变量。带密码的代理地址回显时密码显示为 `__masked__`，原样保存即沿用。

### E. 不要跨环境复制的运行时文件

`jwt.key`（登录态签名）、`data/`（含 CA 证书与缓冲）、`config.json`、`.env`。新机器重新生成即可：`jwt.key` 缺失时自动生成，`data/` 自动创建，`config.json` 从 `config.example.json` 复制。

### F. `go run` 的 BaseDir 行为

`config.BaseDir()` 检测到二进制在临时目录或 `go-build` 缓存里时，回退到**当前工作目录**。所以在项目根 `go run ./cmd/artex` 时，`data/`、`config.json`、`transcripts/` 都落在项目根——这是刻意设计，不是 bug。换成编译好的二进制则落在二进制同级目录。

---

## 9. 换机后的建议顺序

1. 装 Go 1.26.3+ / Node 20+ / PostgreSQL 16
2. 拿代码（§2），装依赖 `cd web && npm ci`
3. 起库、写 `config.json`
4. 跑 §4 的验收；需要数据库的测试以 CI（`test` workflow）的结果为准
5. `./dev.sh` 起来，按启动日志里的**初始化令牌**走 `/setup` → 配 LLM
6. 出包：Actions → **Build Windows amd64** → Run workflow，分支选工作分支，**版本号留空**
7. 验证通过后合入 `main`
