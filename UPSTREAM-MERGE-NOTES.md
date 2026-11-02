# 上游更新核查与合并意见

> 本文档记录：截至 **2026-10-27**，本分支（`fix/all-known-issues`）与上游
> [`linguo2625469/workbuddy2api-panel`](https://github.com/linguo2625469/workbuddy2api-panel)
> 的差异核查结果、逐条合并意见，以及**明确未采纳**的项与理由。

## 一、上游状态

| 项目 | 状态 |
|---|---|
| 上游仓库 | 2026-10-21 起**声明停止维护**（commit `73a7560`「停止维护声明」） |
| 停止原因 | 上游开始禁用反代 + 作者个人精力有限 + 非法转卖泛滥 |
| 披露 | 上游同时声明「近期 issue/PR 处置、代码审查、测试与修复实现大量由 AI 辅助完成」 |
| PR 总量 | 105 个（97 closed / 8 open） |
| `v1.13.0` 之后的合并 | 28 个 PR 合入上游 `main` |
| 停更后新开 | 8 个 PR（#244 / #246 / #247 / #248 / #249 / #250 / #251 / #252）**无人合并** |

上游停更前的最后提交 `73a7560` 已**完整包含**在本分支中（`main` 分支与之逐提交相同，
仅作对照用）。

## 二、核查方法（结论可信度的来源）

比较「上游有什么、本分支有没有」时，用过三种口径，可靠性依次递增：

| 口径 | 可靠性 | 说明 |
|---|---|---|
| `git cherry <branch> upstream/main` | ❌ 不可用 | 会给出 33 个假阳性——本分支以 v1.13.0 为基线重放，上游 commit 被改写 |
| commit message 关键词 grep | ❌ 不可用 | 匹配的是**文本**不是代码，会被同名提交骗 |
| **按代码特征 grep 我方树** | ✅ 可用 | 直接验证功能是否存在 |
| **patch-id 比对** | ✅ 可用 | 唯一能精确判定「是否原样吸收」的口径 |

最终结论以**代码特征 grep**为准（37 个上游 PR 全部命中，见下），patch-id 仅用于
识别「哪些提交是原样 cherry-pick」。

## 三、28 个已合并上游 PR —— 全部已吸收

逐个按代码特征核验，**37 个全部命中**（28 个停更前合并 + 9 个停更后新开）：

```
#123 #133 #138 #143 #144 #148 #152 #158 #159 #162 #178 #185
#196 #199 #202 #203 #204 #205 #212 #213 #217 #220 #221 #223
#225 #230 #232 #233 #244 #246 #248 #249 #250 #251 #252
```

核验用的特征串：`NewMarkupRepair` / `prompt_cache_key` / `creditHistory` / `mobile` /
`dns_servers` / `nameBadIDs` / `expBox` / `realm` / `reasoning_text` /
`enterprise_switch` / `remark` / `upstream_busy` / `compensation` / `cacheMiss` /
`softCooldown` / `6004` / `claim` / `sweep` / `renderExpirySnapshot` /
`search/messages` / `singleflight` / `chat_base` / `11158` / `ProxyFromEnvironment` /
`models.hidden` / `goproxy.cn` / `cross_domain_fallback` / `11-128` / `promo` /
`trial_guard` / `refresh_token` / `rateLimitMeta` / `IsEnterpriseSession` / `wb2api-` /
`客户端接入` / `isolated` / `detail` / `strict`。

## 四、停更后 8 个新 PR 的逐条处置

### 已采纳

| PR | 落地提交 | 说明 |
|---|---|---|
| #244 | `868757b` | 按临期窗口刷新 token。**零冲突**落地 |
| #246 | `96181ba` | issue #241：账号池表体整表空白 + 渲染异常不再静默。**我方本就有同一缺陷**（`rateLimitMeta` 未声明 `detail`），先修自己 |
| #248 | `aac0e71` | 功能早已等价落地（`session.NewConversationID`、`buildCacheKey` → UUID v4、所有 `wb2api-*` 出站点已替换、`DesktopAutomationCreateEvent("自动化")` 已去项目名），**唯一缺口是守护测试**，已补 |
| #249 | README | 「客户端接入」章节（端点一览 / Claude Code 接法 / 模型名 `cn:`/`global:` 前缀语义 / 8 条常见错误对照 / Windows 注意点） |
| #250 | `96181ba` | 三块渲染隔离（模型级限流不再让账号池 / 模型锁池 / 积分到期提醒整块空白） |
| #251 | `96181ba` | 补齐 `rateLimitMeta.detail` |
| #252 | `b2c3ba3` | 采纳**思路**：25 处前端切片测试全部纳入严格模式，提前抓住未声明变量 |

### 未采纳

#### #247 企业形态判定收敛为 `enterpriseId`（issue #240）

**上游主张**：把 `IsEnterpriseSession()` 门控改回 `IsEnterprise()`，理由是
「企业切换会话（`token_source=enterprise_switch`）的行为事件应照常计分
（`heatmap today.score` / `is_active` 随之变化），旧口径把这类可用账号的连登也一起挡掉了」。
上游附了 2026-10-24 的实测（uid `69f23898`，LeisureLinux 实例，`today.score` 0 → 14，
`is_active` False → True）。

**本分支主张**：保留 `IsEnterpriseSession()`，理由是 2026-10-12 的一手实测——
`token_source=enterprise_switch` 的会话，`/v2/report` 明明返回 200，却**被静默丢弃**，
15 个成长任务全部停在 `0/x`，90 秒内每 15 秒回读进度从不跳动。

**为什么仍未采纳**：双方证据指向相反，且**用户当前的 6 个账号 `token_source` 全为空**，
无法在本机复现验证任一方口径。在这种情况下改动判据，等于用一个未经证实的假设替换
另一个未经证实的假设——所以**保留本分支口径**（它至少有一手实测支撑），把分歧明确记录下来。

**这是本分支唯一一处「审阅后明确未采纳」的上游 PR。** 若后续拿到
`token_source=enterprise_switch` 的真实账号，可按下表复验：

| 观测点 | 若支持上游 | 若支持本分支 |
|---|---|---|
| `/v2/report` HTTP 状态 | 200 且 `heatmap.today.score` 递增 | 200 但分数恒 0 / 任务恒 `0/x` |
| `is_active` | 由 False 变 True | 恒 False |

复验脚本：`internal/auth/enterprise_session_test.go`（本分支）
与上游 `internal/auth/enterprise_session_gate_test.go`。

## 五、来自第三方 fork 与本项目工作分支的合并

| 来源 | 合并了什么 | 现状 |
|---|---|---|
| 报告者 fork `misakano7545/workbuddy2api-panel` | 官方客户端出站指纹对齐（双版本真机抓包）、首条消息恒为 system、企业额度「不限量」哨兵（`1<<40`）不污染权重分母、突发积分与连续低缓存率两道快速闸、14003「模型繁忙」独立短窗口 | 已并入 |
| 本项目 `fix/all-known-issues-v1.13.0`（旧 main 基线分支） | 200+ 项修复中的**自研部分** | 已并入；该分支**不再单独维护**（它的唯一独有内容 DSML 已被 `d56376d` 单独移植，且修了上游的守卫 bug） |
| 本项目 `feat/rebase-on-main` | Codex Responses / Anthropic Messages 原生协议 + 官方检索透传 | `4db668d` |
| 本项目 `sweep/*`（7 条） | #104 后台任务+计费、#213 搜索端点、#237/#238 优惠与试用熔断、#244 临期刷新 | 已并入；这些分支**零独有提交** |
| 本项目 `triage/*` | 20 条无 PR 覆盖的 issue 的修复 | 已并入 |

### 报告者 fork 中**尚未**并入的四项（供后续评估）

按代码特征核验，以下四项在报告者 fork 有、本分支没有：

| 内容 | 报告者提交 | 性质 |
|---|---|---|
| 透出上游付费层级 `tier`，切模型不再盲选（issue #235） | `00fea44`（5 文件 +122/-9） | 新功能 |
| 启动补跑同一天只跑一轮，重启不再重打上游 | `2999a8d`（4 文件 +134） | 新功能（有副作用，改排程语义） |
| 状态文件写盘先 `fsync`，读到坏文件回退 `.bak` | `4356a34`（11 文件 +415/-61，含新 `internal/safefile` 包） | 可靠性增强。**本分支只在 `config.json` 上有 `.bak` 回退**（`cmd/server/config.go`），`internal/pool/persist.go` 与 `internal/usage/usage.go` 仍是 `os.WriteFile` + `os.Rename`，**没有 fsync** |
| 首屏主题预热，消除刷新时的深色闪帧 | `5d037aa`（5 文件 +81，新 `internal/panel/theme.js`） | 体验优化 |

前三项需要独立评估（第二项会改排程语义、第三项是新包），**本次未并入**；
第四项是纯前端优化，成本低但收益也低。

### 报告者 fork 2026-10-24 ~ 2026-11-01 新增提交的逐条处置

报告者 fork 在 2026-10-24 之后又补了一批提交（截至 2026-11-01 累计领先 609 个提交），
逐项核对后处置如下。**结论：绝大部分我方已等价或更强**，真正要补的是「出站头对齐」
这一整条线。

| 提交 / issue | 内容 | 我方处置 |
|---|---|---|
| `6562731` / #125 | 6020 带 `retry_after` 时不换号重打 | **已等价**（更强）：我方 6020/6021 都走 `ErrUpstreamBusy` 分支，本身 fail-fast 不轮转，并把 `Retry-After` 原样写进响应头 |
| `192267a` / #128 | chat 流水行补 `request_id` | **已补**（`e06e1ce`）：此前 `chatStat.requestID` 字段存在但全仓无赋值点，`rid=` 段永不出现 |
| `bd0cc1d` / #129 | 登录 client 禁 h2 + 纯传输失败重试 | **已补**（`3891094`） |
| `47d1e2f` / #126 | 运行日志增量游标改用单调序号 `seq` | **不做**：我方 reqlog 是整表拉取的另一设计，`internal/panel/ring.go` 的 `LogEntry` 无 `Seq` 字段（加它要改整个 Ring 与 Snapshot），非同一缺口 |
| `771da0f` / #132 | 事件日志区分客户端原始档位与网关补档 | **不做**：我方 reqlog 与行日志都无 effort 字段，属「思考档位可观测」的新维度而非缺陷修复，成本中等、收益待定 |
| `9b9174b` / #127 | `model_fallback` 支持带域前缀的键 | **不做**：我方无 `model_fallback` 功能（全仓 grep 0 命中），属新功能 |
| `4b7a805` / #130 | 域名被路由策略拒绝（15021）时跨域兜底 | **不做**：我方无 fetch 端点，全仓无 15021 |
| `39fba11` / #131 | 配置页分域编辑 `cn`/`global` 的 `chat_base`/`billing_base` | **已覆盖**：`cmd/server/config.go:153` `Global.ChatBase`、`:163` `CN.ChatBase` |
| `4d7d1e6` / #134 | usage 跨段合并 prompt 取末段值 | **不做**：我方无 `internal/server/continue.go`，无对应功能 |
| `fbe8d3f` / `5677928` / `ee5467c` / `dd2ec95` / `56f479d` / `c8241cc` / `80e275e` | **出站头对齐整条线**：官方真机头夹具、头名逐字拼写、头发送顺序、`Connection` 头、`Accept-Encoding` 四段、真实 TLS 连接复用、**TLS ClientHello 指纹复刻（JA3+JA4）** | **已全部并入**，见下表 |

出站头对齐各段的落地提交与要点：

| 段 | 提交 | 要点 |
|---|---|---|
| 头名逐字拼写 + `Connection` 头 | `61c4463` | 45 个真机头名的大小写映射（`X-Conversation-ID` 而非 `X-Conversation-Id`、`x-stainless-*` 全小写等）。**刻意不做「全部小写化」**——Go 的 `Request.write` 对 `User-Agent`/`Host`/`Content-Length`/`Accept-Encoding` 特判 canonical key，小写会让 UA 回落成 `Go-http-client/1.1`、`Content-Length` 重复发送 |
| `Accept-Encoding` 四段 + report 形态纠偏 | `cd527a8` | `gzip, compress, deflate, br` 会关掉 net/http 的透明解压 ⇒ 必须配套自解压层（`internal/upstream/decode.go`，deflate 同时兼容 zlib 与裸 flate）。chat 链路例外：`ChatHeaders` 显式 `Del` 该头交回 net/http 自动 gzip（SSE 长流解压要动首包计时/空闲监控三处，风险大于收益）。report 事件同步改成官方 Node 形态（原 UA 写死 `Chrome/152`，而 Electron 37 内置的是 Chromium 138，是不可能值） |
| 头发送顺序 + **TLS 指纹复刻** | `a44dd1e` | uTLS 手搓官方 Node/OpenSSL 形态的 ClientHello：18 个 cipher（TLS1.3 三件套排最前）、9 项 sigalgs、groups `[x25519,p256,p384]`、10 个扩展按真机顺序、ALPN 留空。实测 JA3 md5 = `71dc8c533dd919ae9f4963224a4ba8fd`、JA4 = `t13d0a1200_5d04281c6031_b76c5ee2507d`，与官方真机一致（CN 与 GL 同值）。接线要点：**必须接管整条 TLS 拨号**（`DialTLSContext`）——标准库在 `DialContext` 之上自己握手，写进 conn 的是加密记录，那一层看不到也改不了请求头；只有自己握手才拿得到明文 conn，头顺序重排才有落点 |

出站头对齐这一条线为什么重要：**请求头做到 100% 一致也只能骗过 HTTP 层**。TLS 握手
发生在 HTTP 之前，现代风控先看 JA3/JA4 再看 header，所以三段必须一起做。

新增配置项 `upstream.tls_fingerprint`（`node`/`go`，缺省 `node`）：
- `node` = 复刻官方 Node/OpenSSL 形态（生产默认）；
- `go` = crypto/tls 原生形态（排障/回滚用，运行期可切，不必发版）；
- 取值非法**启动即报错**，不静默回落——静默回落等于用户以为复刻生效了。

已知边界：**走 HTTP 代理时指纹不生效**（代理隧道走标准库 `addTLS` 路径，不经过
`DialTLSContext`），此时退回 Go 原生形态。

### 上游 PR #67（Windows 托盘构建）——已吸收并重做

上游 PR #67（`upstream-pr/67`，28 文件 +1090/-28）是我方唯一「有实质代码增量而
此前没有」的未采纳上游 PR。它的目标是消除 Windows 上启动时残留的命令行窗口，
用户侧也确实提了同样的诉求（m18291：「现有的程序通过命令启动，会保留一个命令行
窗口」）。

**上游做法**：`go build -tags tray -ldflags "-s -w -H=windowsgui"` 产出 GUI
子系统二进制，运行时用 `github.com/energye/systray` 常驻系统托盘，右键菜单
「打开Panel」「退出程序」；另带 `wb2api.manifest` / `wb2api.rc` /
`wb2api_windows_amd64.syso`（需 windres 生成）与自建的 `internal/applog/daily.go`
（按天轮转日志）。

**我方没有照搬，重写了三处**：

| 上游做法 | 我方做法 | 为什么 |
|---|---|---|
| `TaskDialogIndirect` 手工打包做「以后不再显示」复选框 | 改用 `MessageBox`（是/否两按钮表达同一意思） | TaskDialog 需要程序清单声明 comctl32 v6，清单要编译进 `.syso` 资源；无 windres 的构建环境生成不出这个资源，`TaskDialogIndirect` 会直接失败降级。MessageBox 是系统原生、永远可用 |
| 自建 `internal/applog/daily.go` 按天轮转 | 复用已有的 `internal/logfile`（按体积上限轮转追加写） | 仓库里已经有能用的轮转日志实现，没必要再引入第二个 |
| 固定「exe 同目录 daily 文件」作唯一 base sink | `cmd/server/logsinks.go`：`logSinks` 抽象 + `safeWriter` 包裹控制台输出 | GUI 子系统没有可靠的控制台句柄，写 `os.Stderr` 会返回「句柄无效」；而 `io.MultiWriter` 遇到第一个写失败就**提前中断**，后面的 sink（面板环形缓冲、日志文件）就再也收不到内容——用户在面板里会看到一片空白而不是错误信息。`safeWriter` 吞掉写错误是这条链路的必要条件 |

**另外三处我方主动改造**：

1. `cmd/server/main.go`：`main()` 拆成 `run() error`，启动失败不再只 `log.Fatalf`
   （托盘版没有控制台，失败信息会彻底丢失），改为弹 `MessageBox`；先 `net.Listen`
   再 `Serve`，端口占用能明确报错而不是静默失败；托盘菜单「退出程序」触发优雅停机。
2. 单实例互斥体 `Local\WorkBuddy2API-Panel-Singleton`：重复双击时第二个实例直接
   打开面板并退出，不会抢端口。
3. 首次启动提示（告知「右键托盘图标」），选「是」后写标记文件
   `.startup-notice-disabled` 到 exe 同目录。

落地提交：`c85140e`（远端 `105130a`）。构建入口 `scripts/build-windows.ps1`
（`-Mode Console|Tray|All`），文档见 `BUILD.md` §2.0 与 README「方式二」。

## 六、上游停更后的维护口径

1. **上游 `main` 只作对照**：本仓库的 `main` 分支与上游 `73a7560` 完全同步，
   **不接收任何修复**。所有修复只进 `fix/all-known-issues`（默认分支）。
   产物口径：**只提供源码，不向 GitHub Release 上传任何二进制资产**，用户自行构建
   （见 [`BUILD.md`](BUILD.md)）；镜像同样只在本机构建，不推送任何镜像仓库。
2. **不再跟进上游**：上游已停更，不会再有新提交。
3. **不再开 PR 回上游**：上游已声明不处理新 issue/PR。
4. **本分支 = v1.13.0 发行版基线 + 上游停更前全部修复 + 本 fork 独有修复**，
   即：用户实际在跑的 `1.13.0` 能直接升级，不需要先升级到上游 `main`。

5. **上游停更 ≠ 上游渠道校验不再加严**：上游的应用侧仍在运行，渠道校验的黑名单
   仍在持续追加。已发生三次：
   | 时间 | 新增黑名单 | 本分支处置 |
   |---|---|---|
   | 2026-09-13 前后 | Claude Code / Codex CLI 的身份句与注入指令句 | `internal/upstream/sanitize.go`（既有 `sanitizeRewrites`） |
   | 2026-10-21 | 本产品（DSH）的身份句 | `internal/upstream/sanitize_dsh.go`（`sanitizeDSHProductName`） |
   | 2026-10-28 | 官方客户端「@ 路径引用协议」说明文案（**两句共现才拦**） | `internal/upstream/sanitize_pathref.go`（`sanitizePathRefProtocol`） |

   三者的共同形态：**请求体里某几句话逐字命中上游的拦截串**，与账号、头、字段无关。
   统一处置原则：识别为渠道校验失败 ⇒ **账号无过错**（不冷却、不禁用、不计入失败），
   并在出站脱敏层把命中句改写到不再逐字匹配。
   维护含义：这类拦截**会继续出现**，每次新增都要按同一套路处理——用真实出站 body
   复现 → 二分/子集实验定位最小触发单元 → 整句改写 → 端到端验证（脱敏前 400、脱敏后 200）。
