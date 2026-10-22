# 使用政策 · 非商业 / 学习项目声明

**Use Policy — Non-Commercial, Learning-Project Only**

本文件是 [WorkBuddy2API Panel](https://github.com/Huiaei/workbuddy2api-panel)（下称「本项目」）
的**项目使用政策**，与仓库根的 [LICENSE](LICENSE) 一并适用。

---

## 一句话

> **本项目是一个学习项目，禁止一切商业行为。**
> 若发现有将本项目用于商业活动的情形，**作者将直接把本仓库私有化，不再公开**。

---

## 1. 项目定性：这是一个学习项目

本项目的定位单一且明确：**个人学习与技术研究**。

- 它是一个**学习成果**，用于理解 OpenAI 兼容网关的实现、流式转发、账号池调度、
  错误分类与跨平台落盘等工程问题；
- 它的**唯一许可用途**是：个人学习、个人自用、个人对自有账号的管理；
- 它**不是**一个产品、一项服务、一个解决方案，也**不承诺**任何可用性、稳定性或支持。

**本项目不提供、也不打算提供任何形式的商业服务。**

## 2. 禁止一切商业行为

**除适用法律强制允许的情形外，禁止将本项目（含全部或部分源码、二进制、镜像、
衍生版本、二次修改版）用于任何商业目的。** 禁止的商业行为包括但不限于：

| # | 被明确禁止的行为 |
|---|---|
| 1 | **售卖**：直接出售本项目或其衍生版本、加壳版、卡密版、授权码版 |
| 2 | **付费中转**：以本项目搭建成「付费 API 中转」「公益服收费」「低价代理」对外收费 |
| 3 | **账号池出租 / 共享收费**：把本项目管理的账号额度对外出租、分销、按次计费 |
| 4 | **捆绑销售**：把本项目与硬件、软件、课程、服务打包售卖或作为付费赠品 |
| 5 | **代充 / 代注册经营**：以本项目为工具开展账号代注册、代充值、号池代运维等经营活动 |
| 6 | **企业内部商业运营**：在公司、工作室、团队内部用于对外提供有偿服务或支撑营收业务 |
| 7 | **引流变现**：以本项目为流量入口，通过广告、引流、私域转化等方式间接获利 |
| 8 | **SaaS 化**：把本项目或其改写版部署为对第三方开放的多租户/托管服务 |

无论是否直接收取费用，**只要目的是获取经济利益或商业优势，即属于本政策禁止的商业行为。**

## 3. 发现商业利用的处置：直接私有化

这是本政策**最重要、也最容易被低估**的一条：

> **一旦发现本项目被用于商业活动，作者将不再进行任何沟通、警告或协商，
> 直接把本仓库转为私有（private），并停止一切公开更新。**

具体含义：

- 仓库**从公开变为私有**后，**公开访问、分支、issue、release、镜像构建将全部停止**；
- **已下载的副本不会被追溯删除**（技术上不可行），但**不会再有更新、修复与安全补丁**；
- 已有的公开历史**保留著作权归属**，作者保留对违规使用追究责任的权利；
- 该处置**由作者单方面决定并执行，不需要事先通知**。

换句话说：**商业利用的代价不是「作者生气」，而是「所有人都失去这个项目」。**
请在做决定前想清楚这一点。

## 4. 关于开源协议的诚实说明

本节是**法律现实**，不是退让，而是为了避免误解：

- 本仓库根目录的 [LICENSE](LICENSE) 是 **MIT 协议**。MIT 明确允许「use, copy, modify,
  merge, publish, distribute, sublicense, and/or sell」——**包括商业使用**。
- 本文件的**非商业政策是项目层面的使用政策与作者意愿表达**，它代表作者的态度与
  后续维护的取舍依据；但**它不能在法律上撤销 MIT 已经授予任何人的权利**，
  尤其是本项目继承自上游的 MIT 授权部分。
- 因此本政策的**实际约束力**来自两条**作者完全可控**的手段：
  1. **不再公开维护**（第 3 条的私有化处置）——这对依赖上游修复的使用者是致命打击；
  2. **不提供任何支持、背书与安全保证**——商用版本出问题与作者无关。
- 如果你需要一份**法律上明确禁止商用**的授权，本仓库当前并不提供。
  若你是使用者且希望有明确法律边界，请**不要使用本项目**；
  若你是作者方且希望改变授权，需要**重新取得继承代码全体著作权人的同意**，
  这不是单方面改一个 LICENSE 文件能完成的。

**我们不打算靠协议条款去打赢官司，我们靠「不再维护」这一条。**

## 5. 如果你的用途是学习或自用

**欢迎。** 本项目对以下用途完全开放，且不收取任何费用：

- 阅读源码、学习实现思路、在自己的项目里借鉴工程做法；
- 管理**你自己的**账号，给自己的本地工具提供 OpenAI 兼容入口；
- 提 issue、提 PR、做 code review、指出文档错误。

这些是**本项目存在的全部理由**。

---

## 6. 与上游的关系

- 本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强分支（fork），
  中间经过 [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)；
- 原始代码的著作权归原作者所有，其授权条款为 MIT；
- 上游 `linguo2625469/workbuddy2api-panel` 已于 **2026-10-21 声明停止维护**；
- **本政策仅适用于本仓库（`Huiaei/workbuddy2api-panel`）自 2026-10 起的维护部分**，
  不改变、也不代表上游各仓库的立场。

---

## English Summary

**This is a LEARNING PROJECT. All commercial use is prohibited.**

This repository exists solely for personal learning and self-hosted use with **your own**
accounts. Any commercial exploitation — selling, reselling, paid API relaying, account-pool
rental, bundling, SaaS hosting, or any other profit-seeking use, direct or indirect —
is **forbidden by this project policy**.

**If commercial use is detected, the author will make this repository PRIVATE without
prior notice and stop all public maintenance.** No updates, no fixes, no security patches.
Everyone loses the project — including legitimate users.

Honest legal note: the root [LICENSE](LICENSE) is MIT, which does permit commercial use.
This policy is a project-level statement of intent and author stance, not a legal
relicensing of MIT-licensed (upstream-derived) code. Its real teeth are the author's
control over *continued public maintenance* — see §4.

Use it to learn, or to manage your own accounts. Nothing else.
