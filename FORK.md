# FORK.md —— 本分支相对上游的差异台账

本仓库是 [`Autumn-27/Norma`](https://github.com/Autumn-27/Norma) 的 fork，
自 **2026-10-13** 起作为**个人维护线**使用：上游不再发版，下游
[`lin9527ya/ARTEX`](https://github.com/lin9527ya/ARTEX) 的 SDK 修复直接落在本仓库，
通过 `go.mod` 的 `replace` 指向本仓库的某个 tag 消费。

## 消费方式（下游）

```go
// ARTEX/go.mod
replace github.com/Autumn-27/norma => github.com/lin9527ya/Norma v0.4.5
```

fork **保留原 module path**（`github.com/Autumn-27/norma`）——这是 Go 的标准 fork
用法：`replace` 右侧按仓库地址取模块，左侧的 import 路径不变，下游零改动。

## 维护约定

1. **每个 tag 自带一次全绿测试**：本仓库新增了 CI（上游没有），push/tag 都跑
   `go vet` + `go test -count=1 ./...`。发版流程 = 改 → 提交 → 推 `main` → 打 tag。
2. **已知缺口显式 skip，不留在红**：文档型缺口测试用 `knownGap(t, why)` 标注并跳过
   （见 `noa/known_gap_test.go`），缺口原因、根因与"改哪里能让它转绿"都写在参数字符串里。
   `NORMA_KNOWN_GAPS=1 go test ./...` 强制执行这些测试（关闭缺口前会失败）——它是
   缺口的可执行清单，也是"缺口被修好后记得删掉 skip"的检查手段。
3. **与上游对账**：若上游复活，`git remote add upstream …` → merge → 按下面差异清单
   逐条确认（尤其 `tool/` 的四处，见 v0.4.4），不要整段覆盖。
4. 差异一律记在本文件，不散落在提交历史里。

## 差异清单

### v0.4.6（2026-10-13）

| 位置 | 差异 | 为什么 |
|---|---|---|
| `tool/task.go` | 新增 `NewManagerIn(base, sessionID)`（`NewManager` 变成 `baseTaskDir()` 的薄封装）；后台任务输出因此可以落在**宿主自己的每次运行目录**里，而不是共享的 `$TMPDIR/norma` | 宿主（ARTEX）的任务工作目录才是那些产物的家：和 spill 输出、执行台账、压缩落盘同根，重启不丢、备份/清理只面对一个根。`$TMPDIR` 既不随宿主生命周期，也可能挂在 tmpfs 上。 |
| `tool/task.go` | 新增 `safeSessionID`：session id 作为路径元素前先净化（分隔符与非法字符→`_`，纯点号→`default`） | id 由宿主传入（可能源自请求参数），`../..` 这样的值原本会让 `filepath.Join` 把目录建到 base 之外。 |
| `agentcore/agentcore.go` | `Options.TaskOutputDir`：给定则用它作为后台任务输出的 base | 上面那条的接线面。空 = 维持 `$TMPDIR/norma`（向后兼容）。 |

> 本轮同时**核实并否决**了一个计划中的改动：P17 原计划"恢复时尊重已持久化的压缩边界"。
> 事实是该机制**上游已实现**——`harness.recordNewBoundary` 会把 boundary marker + 摘要 +
> 保留尾部**作为普通消息**落盘（`boundary` 类型记录只是可观测性标记、重建时跳过），
> `agentcore.TestResumeRestoresCompactedView` 已经钉死了这个行为。按计划改 `transcript.Messages`
> 去重放 boundary 记录会**插入第二个 marker**，被 `TestTranscriptMessagesSkipsBoundary` 当场拦下。
> 结论：不改，记在此处以免下次再撞。

### v0.4.5（2026-10-13）

| 位置 | 差异 | 为什么 |
|---|---|---|
| `llm/retry.go` | `doStream` 在 `Config.HTTPClient == nil` 时回落到 `defaultHTTPClient(cfg.Proxy)`（失败再退 `http.DefaultClient`） | 手工组装的 Config（嵌入方/测试）此前会 nil 解引用 **panic**；现在退化成正常网络错误。修 `TestDoStreamHonorsConfiguredRetries`（上游在 base commit 上就红）。 |
| `noaadapter/compactor.go` | `IsOverflow` 命中溢出时调用 `ParseOverflowWindow` + `overflow.learn` | 学习的窗口此前**没有任何调用者**（`ParseOverflowWindow`/`learn` 是死代码），于是 `Reactive` 永远按"配置里那个已被 provider 拒绝的值"的 95% 设限，重建后的请求还是会被同样拒绝。修 `TestOverflowLearningIsWiredIn`。 |
| `noaadapter/tiering_test.go` | 把"小窗口上 tier 2 不可达"的文档型断言改写成"tier 2 必须持续可用"（测试自己要求的） | 该限制已不成立，原断言在 base commit 上就红。 |
| `agentcore/task_test.go` | `TestBackgroundTaskEndToEnd` 的收尾断言从"Close 删目录"改成"Close 后目录仍在" | 跟随 v0.4.4 第 3 条（见下）。**这是 v0.4.4 引入的回归**：当时只改了 `tool/task_test.go`，这里漏了，新加的 CI 正好把它抓出来。 |
| `noa/known_gap_test.go`、`noaadapter/known_gap_test.go` | 新增 `knownGap` 助手 + 5 处 skip | 见"已知缺口"。 |

### v0.4.4（2026-10-13）

| 位置 | 差异 | 为什么 |
|---|---|---|
| `tool/helpers.go` | `Capture` 的溢出指针恒为**绝对路径**；`spillOutput` 先 `filepath.Abs` | 溢出文件落在 `WorkingDir` 之下时此前回相对路径，而读它的人常不在那个目录（后续命令 `cd` 走了、续跑会话 WorkingDir 不同、模型自己 grep/sed 去读）→ 指针失效。 |
| `tool/helpers.go` | `Capture` 头部截断与 `truncate` 兜底截断按 **UTF-8 rune 边界**对齐（新增 `cutHead`/`cutTail`） | 按字节切会切出半个多字节字符，非法 UTF-8 会进模型上下文与 transcript（部分网关直接拒收）。 |
| `tool/task.go` | `Manager.Cleanup` **不再** `RemoveAll` 会话任务目录 | 后台任务输出与"完整输出已存到 `<路径>`"的指针就在该目录里，会话一关就删 = 把交给模型的指针全变成死链。增长由既有 `staleSessionTTL=24h` 过期清扫兜住。**有意的语义变更**：`tool.TestCleanupRemovesDir` 已反写为 `TestCleanupKeepsTaskDir`。 |
| `tool/task.go` + 三个调用点 | `withEnv(extra)`：进程环境 < 非交互默认（`PAGER`/`GIT_PAGER`/`SYSTEMD_PAGER=cat`、`DEBIAN_FRONTEND=noninteractive`）< 宿主显式传入；覆盖时显式剔除旧条目 | ① pager 会在非交互 shell 上阻塞等待一个没人看的终端，apt 前端会弹交互提示；② `getenv` 取**第一个**匹配，单纯 `append` 同名键并不生效——旧实现正是只 append。 |

## 已知缺口（`NORMA_KNOWN_GAPS=1` 可强制执行）

全部集中在 **noa 的 nudge 节奏**，同一根因：步长常量（`GrowthRatio=0.05` 被
`GrowthFloor=50000` / `GrowthCap=50000` 夹成零宽钳制带；`MinGrowthFloor=20000`）
**不随窗口缩放**——小窗口上"一步"就是半个窗口，抑制一触发就到溢出都抬不起来。

| 测试 | 断言的是 |
|---|---|
| `noa.TestAdaptiveGrowthTracksTheWindow` | 步长应随窗口变化（现在是常量 50000） |
| `noa.TestSuppressionReleaseFitsInsideTheWindow` | 抑制释放门槛应 ≤ 窗口的 25% |
| `noaadapter.TestMostlyCompetentModelStaysBounded/seed2024` | 偶发被忽略的 nudge 不应把长会话顶出窗口 |
| `noaadapter.TestLongSessionStaysBounded` | 长会话最后十轮峰值 ≤ 窗口 75% |
| `noaadapter.TestLongSessionWithPartiallyCompliantModel` | 部分配合的模型下机械阀门仍守得住窗口 |

**根因是规格自相矛盾**：上游留了一次做到一半的迁移——pacing_test.go 要求自适应，
而 `TestDefaultConfigMatchesSpec`（逐字 pin 50000/20000）、`TestResolveAdaptiveGrowth`
（pin 200k 窗口 = 50000）、`TestNudgeHoldsBelowCadence`、
`TestPressureBandDoesNotRenudgeEveryFewTurns` 四条又在 pin 旧的常量行为。
选哪一边是**算法决策**（会改变所有 noa 会话的 nudge 频率），不该由某次顺手修测试决定，
所以这里保持上游的出厂行为不变、把缺口显式挂起。关闭路径：放宽钳制带
（例如 `GrowthFloor: 5_000` / `MinGrowthFloor: 2_000`）+ 同步更新上面那四条 pin 旧值的测试，
然后删掉这 5 处 `knownGap`。
