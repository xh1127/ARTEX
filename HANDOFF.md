# ARTEX 开发交接文档（换机迁移）

> 生成时间：2026-11-18 · 来源机器：Windows 10 Pro (win32, Git Bash)
> 源工作区：`D:\code\ARTEX` · 分支 `main` · HEAD `51291fe Create manual-build.yml`
> 上游：`origin` → `https://gh-proxy.org/https://github.com/xh1127/ARTEX.git`

这份文档解决三件事：**带什么走**、**新机器怎么起来**、**当前手上没提交的活是什么**。

---

## 0. 一分钟清单

| 要做的事 | 命令 / 说明 |
|---|---|
| 带走代码 | 推荐 `git push`（见 §2-A）；不行就用仓库根的 `artex-wip.patch`（见 §2-B，已实测可干净应用） |
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

### 方案 A（推荐）：走 git

```bash
# 源机器：当前 28 个文件的改动先落到一个分支
git switch -c feat/llm-user-agent
git add -A
git commit -m "feat(llm): per-profile User-Agent + settings txn + provider hot-reload hardening"
git push -u origin feat/llm-user-agent
```

```bash
# 新机器
git clone https://gh-proxy.org/https://github.com/xh1127/ARTEX.git
cd ARTEX
git switch feat/llm-user-agent
```

> 若不想推上游仓库，也可以在源机器 `git bundle create ../artex.bundle --all`，新机器 `git clone artex.bundle ARTEX`。bundle 是单个文件、含完整历史，比拷 `.git` 目录稳。

### 方案 B（备用）：用已生成的补丁

仓库根已有 `artex-wip.patch`（141 KB，**28 个文件**，含 9 个新增文件）。

它是用 `git diff --binary` 在 `git add -N`（intent-to-add）之后生成的，因此**新增文件也包含在内**。已在干净的 `HEAD` 检出上实测：

```
git apply --check artex-wip.patch   → APPLY_CHECK_OK
git apply      artex-wip.patch      → 28 files changed
```

新机器上：

```bash
git clone <仓库> && cd ARTEX
git checkout 51291fe            # 必须正好是这个提交，补丁基于它
git apply artex-wip.patch
```

**用完请删除 `artex-wip.patch`**——它没有被 `.gitignore` 覆盖，很容易被 `git add -A` 顺手提交进去。或者自己加一行 `/artex-wip.patch` 到 `.gitignore`。

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
- **静默跳过**：`requestHeadersDatabase()`（`server/llm_request_headers_test.go:14`）在 DSN 为空时 `t.Skip`。本次改动新增的 8 个测试文件几乎全靠它，因此**没设 DSN 就等于没测**。

> CI 现状：`.github/workflows/` 只有 `release.yml`（打 tag 时构建五平台）和 `manual-build.yml`（手工构建 Windows amd64），**两个都没有 `go test` 步骤**。想在 CI 上跑测试需要自己加一个带 `postgres:16` service 的 job。

---

## 5. 当前未提交的改动（28 个文件，+2206 / −370）

这是换机后**第一件要接着做的事**。改动是一条主线加三块顺带加固：

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

## 6. 已知问题与待办（按建议优先级）

以下都是本次审查得出的结论，不是猜测；行号以当前工作区为准。

### 高

1. **`deepSeekSearchCreds` 用「激活配置」而非「调用方绑定的配置」** — `server/manager.go:576`
   `WebSearchOpts()` 没有 agent 上下文，被 pin 到便宜 profile 的任务，搜索请求走的是另一个 profile 的 key/base_url/UA。费用记错对象，网关按 UA 的风控/限流分组也会错位。属既有问题，UA 特性让它更明显。
2. **测试在 CI 上永远跳过** — 见 §4。新增的并发修复没有回归保护。建议加带 `postgres:16` service 的 `go test ./...` job。
3. **「已落库但回 500」的部分成功语义** — `server/server_mgmt.go:1875` 等处
   配置已提交、内存态已发布，却返回 500，前端按失败处理。同一模式出现在 `pgSaveLLMRetryPolicy`、`pgDeleteProfile`、`pgActivateProfile`、`putSettings` 的 `traffic.SetUpstreamProxy` 分支。建议改成 `200 + warning` 字段。

### 中

4. **`configureWebSearch` 绕过后置处理** — `agent/websearch.go:28`
   它 append 到 `opts.Tools`，位于 `AugmentTools` 的 `ToolResolve`（DB 工具表过滤/描述覆盖）、`findingWorkflowTools`、`guardPanic` **之后**。后果：工具页禁用 `web_search` 无效，也不被 panic 兜底。
5. **`configureWebSearch` 静默吞错**：`newWebSearch` 失败时工具凭空消失、无日志。配置错了（UA 非法、代理串错）只能靠人猜。建议 `log.Printf`。
6. **`putSettings` 直写 Manager 私有字段，留下 7 个死 setter**：`SetWebSearch`、`SetGlobalProxy`、`SetLLMRecordEnabled`、`SetNoaCompaction`、`SetWorkers`、`SetLLMPoolEnabled`、`SetLLMPoolBindFallback` 现在只有测试在用。其中 `SetGlobalProxy` 自带 `syncBrowserMCPProxy()` 副作用，将来谁改回 setter 就会与 handler 路径分叉。
7. **激活配置构建失败会整站停摆且 UI 看不到原因** — `server/server.go:458` `s.llmOn = err == nil && prov != nil`。语义更正确，但 `getLLM` 只回 `configured:false`、没有 error 字段。
8. **`applyLLM` 的 busy-retry 无上限** — `server/server.go:419` 的 `for { ... continue }`。频繁保存配置理论上是忙等。
9. **`reapplyActiveProfile` 把 `s.llmCfg` 当 env fallback** — `server/server.go:381`。删除激活配置后会拿刚删掉那个配置的副本重建，日志来源变成空字符串。

### 低

- `ProbeWebSearch` 里的 `t.(*deepSeekSearchTool)` 强制断言可去掉（backend 已判过）。
- `max_uses: min(max((limit+9)/10, 1), 3)` 建议加一行注释。
- `db/schema.sql` 新列没加 CHECK（同表 `max_tokens` 有 `CHECK >= 0`），512 限制只在 Go 层。
- **`CHANGELOG.md` 的 `[Unreleased]` 是空的**——本次改动量与 0.3.15 任一条相当，建议补。
- `server/server.go` 已 3971 行，`putSettings` 单函数约 200 行，建议拆三段。
- `pgListModels` 用裸 `&http.Transport{}`（无 Timeout），与 `quotaAwareHTTPClient` 不一致。

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

### B. 前端 dev 端口与文档不一致

`dev.sh:19` 与 `README.md:275` 都打印 `http://localhost:5173`，但 `web/package.json` 的 `dev` 脚本是裸 `next dev`，**没有 `-p`**，Next 16 默认跑 **3000**。仓库里也找不到任何 `.env.local` 或端口配置。

→ 实际访问 **http://localhost:3000**。要么改文档，要么在 `web/package.json` 里加 `"dev": "next dev -p 5173"`。这是文档缺陷，换机后第一次跑就会遇到。

### C. 端口占用

`:8787`（后端+前端 API）、`:8788`（录制代理，仅监听 127.0.0.1）、`:5432`（Postgres）。`docker-compose.bench.yml` 会再占一套，基准测试前先停掉常规服务。

### D. 代理相关（三套，别混）

| 配置项 | 作用范围 |
|---|---|
| 每个 LLM profile 的 `proxy` | 只影响该 profile 的模型请求 |
| `global_proxy`（系统设置） | 所有**目标**流量；录制开启时成为 MITM 的上游 |
| `web_search_proxy`（系统设置） | 只影响搜索后端出口 |

留空 = 直连（**不回退** `HTTP_PROXY`/`HTTPS_PROXY` 环境变量，`agent/provider.go:436` 显式 `transport.Proxy = nil`）。

### E. 不要跨环境复制的运行时文件

`jwt.key`（登录态签名）、`data/`（含 CA 证书与缓冲）、`config.json`、`.env`。新机器重新生成即可：`jwt.key` 缺失时自动生成，`data/` 自动创建，`config.json` 从 `config.example.json` 复制。

### F. `go run` 的 BaseDir 行为

`config.BaseDir()` 检测到二进制在临时目录或 `go-build` 缓存里时，回退到**当前工作目录**。所以在项目根 `go run ./cmd/artex` 时，`data/`、`config.json`、`transcripts/` 都落在项目根——这是刻意设计，不是 bug。换成编译好的二进制则落在二进制同级目录。

---

## 9. 换机后的建议顺序

1. 装 Go 1.26.3+ / Node 20+ / PostgreSQL 16
2. 拿代码（§2），装依赖 `cd web && npm ci`
3. 起库、写 `config.json`
4. **跑 §4 的验收**——特别是带 `ARTEX_PG_DSN` 的 `go test ./...`，这是本次改动第一次真正被验证
5. `./dev.sh` 起来，走 `/setup` → 配 LLM → 在「LLM 配置」页验证 UA 字段（连接测试 + 加载模型都应带新 UA）
6. 把 §5 的 28 个文件整理成一个提交；补 `CHANGELOG.md` 的 `[Unreleased]`
7. 按 §6 处理高优先级三条
8. 删除 `artex-wip.patch`，或加进 `.gitignore`
