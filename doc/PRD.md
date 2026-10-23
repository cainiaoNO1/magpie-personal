# 需求文档 · WorkBuddy 免费额度自由化

> 版本 v1 · 2026-10-22 · 状态：待评审
> 范围：仅 WorkBuddy / WorkBuddy AI 免费额度相关能力（见 §3 非目标）

---

## 1. 背景

### 1.1 部署现状

| 服务器 | 位置 | 规格 | 承担的额度模型 |
| --- | --- | --- | --- |
| `113.45.68.252` | 美国 | 2 核 / 2G / 2M | 5 小时限额 + 7 天限额，自带功能反代 Gemini |
| `18.188.150.249` | 广州 | 2 核 / 2G / 2M | WorkBuddy 插件反代 `deepseek-v4.1-flash`，按账号积分计量 |

当前在 `18.188.150.249` 上挂了 **7 个 WorkBuddy 账号**，共享免费模型额度。

### 1.2 WorkBuddy 免费额度的真实机制

单账号：

- 24 小时内允许使用 **100M token**；
- 用完后上游返回拒绝（用户观察为「499」，见 §7 待验证 A1）；
- 需要等到**本轮第一次会话起满 24 小时**才刷新，而不是固定时刻重置。

10 月 31 日前有 3 个免费模型。额度耗尽后账号进入不可用状态，直到刷新。

### 1.3 代码现状（证据）

WorkBuddy 与 WorkBuddy AI 在上游已**退役为社区插件**，不在本仓库实现：

- `AGENTS.md` 退役表列出 `workbuddy` / `workbuddy-ai`；
- `internal/provider/migrate_workbuddy.go:18` 指向 `@magpie-community/opencode-workbuddy-auth`；
- `internal/provider/workbuddy_checkin.go:1-10` 明确写「A fix to the plugin's own paths belongs in github.com/magpie-community/plugins (packages/workbuddy)」。

插件已克隆至 `/Users/ymj/Desktop/code/magpie-plugins/packages/workbuddy`（v0.1.29，`index.mjs` 1858 行）。

插件当前上报的用量形状（`index.mjs:1290` `usageOf`）：

```js
{ plan, windows: [{ name: "Credits", used, display, amount, limit, unit: "credits",
                    runsOutAt?, runsOut?, notModels? }] }
```

关键实现点：

| 位置 | 行为 |
| --- | --- |
| `index.mjs:1721` `usage(getAuth)` | 读 `/billing/meter/get-user-resource-summary`，企业账号回落到 `get-enterprise-user-usage` |
| `index.mjs:1357` `packagesOf` | 免费包按**北京当天**切片：`SlicePeriodStartTime = 当天 00:00:00`、`EndTime = 当天 23:59:59` |
| `index.mjs:1351` `SLICE = "4"` | `CapacityType 4` 视为「每日切片，各账号同一午夜重置」 |
| `index.mjs:1399` `runsOutOf` | 主动跳过 `DAILY_CODE` 与 `SLICE` 包，不参与排序 |
| `index.mjs:1796-1802` | 识别 `code 14018` / "Credits exhausted" → 加 `EXHAUSTED_HINT`，标为 `insufficient_quota` |
| `index.mjs:1826` `explained` | 该错误原样透传给 magpie，**不改变路由状态** |

magpie 侧对应设施：

| 能力 | 位置 |
| --- | --- |
| 额度窗口结构 | `internal/provider/subscription_usage.go:29` `QuotaWindow`（含 `Amount`/`Limit`/`Unit`/`RunsOutAt`） |
| 账号用量记录 | `internal/usage/usage.go:26` `Record`，含 `ProviderAccount` |
| 账号休息/冷却 | `internal/gateway/routing.go:136-150, 464-503` `restingUntil`（`cooldown`/`backoff`） |
| 额度接口 | `internal/gui/usage.go:180` `/api/usage/quotas`、`:205` `/api/usage/quotas/refresh` |
| 统计周期写死 | `internal/gui/assets/app.js:16716` `PANEL_USE_PERIODS = today/7d/30d/all`；`:22002` Today/Yesterday 日标签 |

---

## 2. 问题陈述

### G1 — 免费额度没有被建模

两侧都只认「credits」，不认「100M token / 24h / 账号」。

- 插件把免费包当成 credits 窗口上报，且对 `CapacityType 4` 假定**北京午夜**重置；
- 用户观察到的真实规则是**首次会话起滚动 24 小时**。

两者不一致，导致任何「还剩多少、何时恢复」的推算都会偏。

### G2 — 耗尽信号没有进入路由

插件识别出 `14018` 并附上人话提示，但只是把错误**透传**给调用方。magpie 的路由层不知道这个账号已经废了，下一个请求仍可能挑中它，再撞一次墙。

`restingUntil` 已有成熟的 `cooldown`/`backoff` 机制，但没有接上这个信号。

### G3 — 查询被锁死在固定时间段

- 用量页周期写死 `today / 7d / 30d / all`（`app.js:16716`），不能任意区间；
- **后端其实已经支持**：`POST /api/usage/quotas/refresh?provider=X&user=Y` 可按单账号刷新（`usage.go:205-215`），前端也已有 `quotaRefresh(q)` 按钮（`app.js:17942`）；
- 真正的缺口是**展示层的时间区间写死**，而不是缺少按账号查询的能力。

> 这是好消息：G3 的修复面比预想小，主要是前端区间控件。

### G4 — 看不到「什么时候解冻」

账号被额度挡住后，面板没有「已冻结 / 预计 X 小时后恢复 / 依据是什么」的呈现。7 个账号轮转时，只能靠人工试错。

---

## 3. 目标与非目标

### 目标

1. 按账号、在任意时刻查询剩余可用量；
2. 把「100M / 24h 滚动窗口」建成 magpie 能理解、能展示、能排序的一等数据；
3. 额度耗尽信号进入路由，自动跳过并自动恢复；
4. 面板呈现冻结原因与恢复时间。

### 非目标（本轮不做）

- 5 小时 / 7 天限额模型（美国服务器）——后续追加章节；
- GitHub 仓库创建与推送——本轮只产文档，见 `doc/tasks.md` 阶段 4；
- 部署与运维流程；
- 修改 magpie 的计费/账单逻辑。

---

## 4. 术语

| 术语 | 含义 |
| --- | --- |
| **轮次（round）** | 一个账号从「首次使用免费额度」到「满 24 小时刷新」的周期 |
| **滚动窗口** | 以轮次首次会话为起点、长度 24h 的窗口，非自然日 |
| **冻结** | 账号额度耗尽，被路由跳过 |
| **解冻** | 窗口到期，账号重新可被选中 |
| **权威信号** | 上游明确返回的耗尽答复（`14018` / `insufficient_quota` / 499），优先于本地推算 |

---

## 5. 需求清单

### R1 · 任意时刻按账号查询

**要求**：面板能在任意时刻对**指定账号**发起一次即时额度刷新，不受 `today/7d/30d` 周期限制；支持自定义时间区间查看该账号的历史用量。

**验收**：

- [ ] 用量页可对单个账号点击刷新，返回该账号最新额度；
- [ ] 存在自定义区间输入（起止时间），可查询任意跨度；
- [ ] 原有的 `today/7d/30d/all` 快捷项仍然可用（向后兼容）；
- [ ] 刷新失败时有明确错误提示，不静默。

### R2 · 免费额度按 token 计量

**要求**：除插件上报的 credits 窗口外，magpie 额外为 WorkBuddy 账号维护一个「token / 滚动 24h」窗口，数据源为 magpie 自己经手的用量记录（`usage.Record.ProviderAccount`）。

**验收**：

- [ ] 每个 WorkBuddy 账号在额度卡片上显示「本窗口已用 X / 100M token」；
- [ ] 窗口起点取该账号本轮首次请求时间，满 24h 自动滚动；
- [ ] 该窗口与插件上报的 credits 窗口**并列展示**，不互相覆盖；
- [ ] 无用量记录时显示为未知，而不是 0。

### R3 · 捕获权威耗尽信号

**要求**：上游返回额度耗尽时，标记该账号冻结，并记录本轮起点。

**验收**：

- [ ] 识别 `code 14018`、"Credits exhausted"、`insufficient_quota`；
- [ ] 识别上游 HTTP 499（若验证 A1 成立）；
- [ ] 冻结后该账号不再被路由选中；
- [ ] 冻结事件写入日志/面板，含触发时间与原始错误码。

### R4 · 冻结与自动解冻

**要求**：冻结持续到本轮起点 + 24h，到期自动解冻；复用现有 `restingUntil` 机制。

**验收**：

- [ ] 冻结时长 = 首次会话时间 + 24h，不是固定冷却；
- [ ] 到期后账号自动回到候选池，无需重启或人工操作；
- [ ] 解冻时间可在面板查看；
- [ ] 未到期的账号在路由决策中被跳过，且跳过的原因可追溯。

### R5 · 面板呈现

**要求**：展示每个 WorkBuddy 账号的当前状态。

**验收**：

- [ ] 显示状态：正常 / 已冻结 / 未知；
- [ ] 显示「额度用尽 · 周X HH:MM 恢复 · 还有 N 小时」；
- [ ] 显示本轮已用 token 与剩余；
- [ ] 多账号可按「最快恢复」或「剩余最多」排序。

### R6 · 与现有排序共存

**要求**：不破坏插件已有的 `runsOutAt` 排序（快到期先用完）与 `notModels`（免费模型不计入 credits）语义。

**验收**：

- [ ] 现有 `spareFree` / `runsOutOf` 行为不变；
- [ ] 新增的 token 窗口不覆盖 credits 窗口的 `notModels`；
- [ ] 插件侧相关测试（`usage.test.mjs`）保持通过。

### R7 · 文档与可维护性

**要求**：改动符合上游规范，便于后续同步 upstream。

**验收**：

- [ ] 触及 `docs/subsystems/` 中已文档化子系统时同步更新对应页面；
- [ ] 自用文档集中在 `doc/`，不与上游 `docs/` 混放；
- [ ] 新代码遵循 `docs/code-standards.md`。

---

## 6. 落地分工

| 层 | 仓库 | 承担 |
| --- | --- | --- |
| 插件侧 | `magpie-plugins/packages/workbuddy` | 暴露更精确的免费额度字段、上报轮次起点 |
| magpie 侧 | 本仓库 | token 计量、冻结/解冻、路由接入、面板 |

设计见 `doc/design.md`，任务拆解见 `doc/tasks.md`。

---

## 7. 待验证假设

| 编号 | 假设 | 验证方式 | 不成立时的影响 |
| --- | --- | --- | --- |
| **A1** | 用户观察到的「499」是上游返回的 HTTP 状态码 | 在 `18.188.150.249` 抓一次真实耗尽请求的响应 | R3 的判定条件需改为纯 `14018` |
| **A2** | 刷新起点是「本轮首次会话」，不是北京午夜 | 观察一个账号耗尽后首次可用时间，与首次使用时间比对 | 若为午夜重置，R4 退化为固定时刻解冻，实现更简单 |
| **A3** | 100M 的计数口径 = 请求 token 总量（含缓存） | 对比插件 credits 与 magpie usage 记录的累计值 | 需调整计量字段 |
| **A4** | 免费模型的额度与付费 credits 是**独立**的 | 查 WorkBuddy 官方说明或实测 | 若共享，R2 的窗口需与 credits 合并 |

> A2 是**影响最大**的假设：它决定 R4 是「滚动窗口」还是「固定时刻」，也决定插件侧是否需要改 `SLICE` 的处理。建议优先验证。

---

## 8. 已排除方案

| 方案 | 排除原因 |
| --- | --- |
| 只做被动捕获，不建本地计量 | 无法提前预警，且面板没有剩余量可显示（Q2 已否决） |
| 只做本地计量，不接权威信号 | 阈值与官方口径可能不一致，会误判（Q2 已否决） |
| 把插件 vendor 进本仓库 | 与上游 retired 策略冲突，同步 upstream 持续冲突（Q5 已否决） |
| 新建独立「账号健康面板」页面 | 工作量与回归面大，且 `/api/usage/quotas` 已够用（Q3 已否决） |
| 固定冷却 + 指数退避 | 早于真实刷新时间，会反复撞墙（Q6 已否决） |
