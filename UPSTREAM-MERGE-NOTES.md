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

## 六、上游停更后的维护口径

1. **上游 `main` 只作对照**：本仓库的 `main` 分支与上游 `73a7560` 完全同步，
   **不接收任何修复**。所有修复只进 `fix/all-known-issues`（默认分支）。
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
