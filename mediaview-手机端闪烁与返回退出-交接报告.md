# mediaview 手机端「文件夹闪烁 → 返回退出」问题 —— 交接报告

> 交接对象：接手排查的工程师 / AI
> 交接时间：2026-11-08
> 当时版本：v2.4.37（commit `c980a61`）　·　结局版本：**v2.4.40**
> 结论先行：**已解决（2026-11-08，v2.4.40）—— 但不是靠修守卫，而是把 SPA 整个退回真页面导航。**
>
> ## ★ 结局（先读这一段，再决定要不要往下读）
>
> 2.4.36 ~ 2.4.39 四版都在「历史层加守卫」，全部失败。最终采用的方案是本报告 **§10 的退路 B**：
> **换回真页面导航（`location.href`）+ 目录快照**，并把守卫体系、DOM 目录存档、
> 点按预取、连接池取消等一整套 SPA 专用代码**全部删除**（净减 1600+ 行）。
>
> 原因很干脆：SPA 下整个会话只有一条历史，宿主的手势返回必然落到应用之外 ——
> 这不是"层数不够"或"pushState 不可靠"能解释的问题，是模型选错了。
> 真导航下每层目录本身就是一条历史条目，`canGoBack` 永远为 true，问题从根上不存在。
>
> 实测（`tools/realnav_probe.js`，headless Edge，`/api/list` 故意延迟 600ms，关掉 bfcache）：
> 首访 780ms / **返回 56~69ms / 前进 56~82ms**，三处"内容出现后又变空白的帧"全为 0；
> 滚动位置 300 → 300 还原。
>
> 下方 §1~§9 保留全部排查过程与**已排除的方向**，作为"别再走这条路"的记录；
> §10 的三条退路里，**B 已被验证可行，A/C 不必再试**。

---

## 阅读顺序

| 章节 | 作用 | 谁必读 |
|---|---|---|
| §1 完整时间线 | 这个问题是**从"修闪烁"一路演变过来**的，不看这段会以为只是一个守卫 bug | 所有人 |
| §2 三角约束 | 为什么它难：三个需求互相牵制，牺牲任何一个都会退回老问题 | 所有人 |
| §3 取证（先做这个） | 三版失败的根源是没现场数据就开改 | 接手人 + 用户 |
| §4–§6 宿主事实 / 已排除方向 / 候选根因 | 不要重复我走过的路 | 接手人 |
| §7–§9 代码地图 / 构建 / 红线 | 动手前必看 | 接手人 |

---

## §1 完整时间线（问题和修法是纠缠在一起的）

用户的目标从一开始就是**三条同时成立**（原话）：
> 「做到能适配手势返回并不退出软件，而且没有加载和闪烁情况」

下面每一步都是为了让其中一条变好，代价是另一条变差。

### 2.4.29 — 手机竖图连续流（与本题无关，但是后面所有判断的背景）
点图进单列瀑布流，退出时回到网格里"你看的那一张"。建立了一个前提：
**手机端的浏览是"页内状态"，不是"页面跳转"**。

### 2.4.30 — 竖图流图源统一（与本题无关）

### 2.4.31 — **修「进/退文件夹会闪一下」**（本次交接的起点）
两个独立根因，都是"消失与出现不在同一帧 / 内容比数据晚到"：

- **桌面档（内部换目录）**：`layout()` 删完格子后只 `requestRender()`，重建等下一帧 rAF
  —— 中间那一帧就是"格子全没了"的空画面（实测第 4 帧 `children=0`，第 5 帧才恢复）。
  改成 `renderNow()` **同步重建**，删除与重建落在同一任务里。
- **手机档（整页导航）**：触屏换目录是 `location.replace()`，新页面首屏必须等
  `/api/settings` + `/api/list`。新增**目录快照**（`sessionStorage['mv_snap:<path>']`，
  10 分钟过期 / 6 个目录 / 单目录 ≤800 项 ≤700KB），`boot` 的 `?dir=` 分支**不等 settings**
  先用快照铺，真实列表回来用 `sameItems` 判断"一致就完全不重建"。
- 加载指示阈值 180 → **400ms**（NAS 换目录常在 200~350ms 返回，180ms 会让转圈闪一下又消失）。

实测（headless Edge 390×844，网络加 300ms 延迟）：

| 场景 | 首屏出现格子 | 格子数变化 |
|---|---|---|
| 首次打开目录（无快照） | 1010ms | 0 → 9 |
| **返回父目录（有快照）** | **352ms** | **0 → 9（一步到位）** |
| 再次进子目录（有快照） | 353ms | 0 → 4 |

⚠️ **注意这里的伏笔**：此刻"换目录"仍然是**整页导航**（`location.replace`），
所以"返回"每次都是整页重载 —— 不闪了，但**慢**，而且 `replace` 不压历史。

### 2.4.32 — 竖图流定位失效（与本题无关）

### 2.4.33 / 2.4.34-probe — 导航探针版
目的：搞清楚**宿主的返回到底是什么机制** —— 是让 WebView 退历史（能收到 `popstate`），
还是直接销毁页面（只能收到 `pagehide`）。结论：**`popstate` 能收到**，宿主在退 WebView 历史。
（探针面板 `probe_nav.js` 至今仍在包里，`index.html:515` 引用。）

### 2.4.35 — 手机端换目录改**内部切换**（零重载）+ 封面跨会话记忆
把 `location.replace` 换成 SPA：只发一次 `/api/list`，界面零空窗、内存缓存不丢，
`replaceState` 同步 `?dir=`。**"加载"和"闪烁"两个问题到这里才算真正解决。**

⚠️ **副作用（关键）**：真导航时，每次换目录都是**新文档**，历史被重置成
「1 页 + 1 守卫」，宿主的返回永远只退一格就到底 —— 问题被"重置"掩盖了。
改成 SPA 之后历史不再重置，**"第二次返回就退出"立刻暴露**。

### 2.4.36 — 假定"一次手势连退两格"，守卫 1 层 → 2 层
`MV_GUARD_DEPTH = 2` + 320ms 去抖（第二格只补守卫、不再退层）。
**结果：手感毫无变化。**

### 2.4.37 — 假定"守卫压根没进历史"，回读 + hash 降级 + 轮询 + 返回事务
`pushState` 后回读 `history.state`；失败降级 `location.hash = #mvNg`；
前台 400ms 轮询 + `touchstart` 对齐；深度 3；行为层改成提交式事务（180ms 静默窗）。
**结果：仍然两次滑动退出。**

---

## §2 三角约束（这是本题难的原因）

```
        ① 不闪烁
        ↗        ↖
   ② 不重载  ←→  ③ 手势返回不退出
```

| 想要 | 必须做的 | 代价 |
|---|---|---|
| ① 不闪烁 | 删/建同帧（`renderNow`）+ 内容不等网络（目录快照） | 快照只对"刚才去过"的目录有效，冷目录仍要等 |
| ② 不重载 | SPA 内部切换（`replaceState` 改 URL，不压历史） | **历史恒定 ⇒ 宿主可退条目变少** |
| ③ 返回不退出 | 历史里始终有属于本应用的条目 | 要么压守卫（2.4.36/37，**无效**），要么让每层都是真页面（⇒ 违反 ②） |

**所以不能简单地"回退到 2.4.34 的真导航"来修 ③** —— 那会把 ② 重新破坏掉。
任何方案都必须回答：**在不重新整页加载的前提下，怎么让宿主的 `canGoBack` 永远为 true。**

这也是为什么我建议先取证：如果 ③ 的根因是"守卫根本没压上"，
那这个三角是有解的（换个可靠的方式压）；如果是"宿主的判断不看 WebView 历史"，
三角无解，只能改交互模型。

---

## §3 先做这件事：取证（不要先改代码）

前面三版失败，主要原因是**在没有现场数据的情况下连续猜**。请先让用户提供下面 5 项。

| # | 要什么 | 怎么拿 | 为什么关键 |
|---|---|---|---|
| 1 | 探针面板**第一行截图**（滑动 1 次后、第 2 次之前各一张） | 面板常驻页面底部，形如 `boot #1 · hist=5 · 守卫 3/0 [state] · tx:upLevel×1` | 直接回答"守卫到底压上了没有" |
| 2 | 第二次滑动时，**直接回桌面**，还是**先弹「退出急速媒体浏览？」再退出** | 观察 | 两者根因完全不同（§6 的 A/B 分支） |
| 3 | 当时在第几层（面包屑文字） | 页面顶部面包屑 | 判断"第二次返回"是否其实已在**聚合根层** |
| 4 | 面板事件列表里，第 2 次滑动**有没有 `popstate HOST` 这一行** | 面板下半部分 | 没有 ⇒ 宿主走自己的栈，网页侧无解 |
| 5 | 测试用的 App 版本 | 设置 → 关于 | 确认装的是不是最新包 |

**若面板显示 `守卫 3 [state]` 但仍退出，且第 2 次有 `popstate`** —— 直接跳 §6-C。

---

## §4 宿主环境（APK 静态分析，可当真）

- 用户提供过 `Y:\base.apk`（飞牛 Android 客户端，131 MB）。
- 技术栈：**Flutter + flutter_inappwebview**。
- `lib/arm64-v8a/libapp.so`（Dart AOT）里：
  - `canGoBack` 出现 **1 次**，`goBack` 出现 **1 次** —— 宿主只有一处返回处理，
    不存在"两套逻辑各调一次"，**这削弱了"一次手势连退两格"的假设**（2.4.36 正是按那个假设做的）。
  - `onWillPop` / `PopScope` / `canPop` / `maybePop` 均未出现；`willPop` 1 次（可能是依赖库）。
  - `再次点击退出应用` 在 arm64 libapp.so 里**不存在**（早期分析提到的这条不成立，别当证据）。
  - `SystemNavigator` 2 处（`SystemNavigator.pop()` = 退出到桌面）。

自测：
```python
import zipfile
b = zipfile.ZipFile('Y:/base.apk').read('lib/arm64-v8a/libapp.so')
for p in ['canGoBack','goBack','PopScope','onWillPop','SystemNavigator','退出']:
    print(p, b.count(p.encode()))
```

---

## §5 已试过、且**确定无效**的方向（不要重复）

判定标准只有一个：**真机上连续滑动 2 次不退出。** 所有版本的单测/打包/冒烟都是绿的。

| 版本 | 做法 | 结果 |
|---|---|---|
| ≤2.4.34 | 换目录走真页面导航（`location.replace`），靠宿主页面栈记账 | 不闪了（快照 352ms），但 `replace` 不压历史，本身就不产生可退条目 |
| 2.4.35 | SPA 内部切换，历史恒定 | 零重载达标；**返回退出暴露** |
| 2.4.36 | 守卫 1 层 → **2 层** + 320ms 去抖 | **手感毫无变化** ← 决定性线索 |
| 2.4.37 | 压完**回读** state、失败降级 **hash**、400ms **轮询**、深度 3、**提交式事务** | **仍然两次滑动退出** |

### 「加层数没变化」为什么是决定性线索

守卫若真实存在，1 层和 2 层手感**必然不同**。完全一样只有一个解释：
**守卫条目一条都没真正进入历史**（0 层 × N = 0 层）。

2.4.37 就是按这个推理做的。既然仍失败，只剩三种可能，面板读数可一次区分（§6）。

---

## §6 三个候选根因与对应实验

### A. 守卫压根没进历史（面板 `守卫 0` / `[未压上]`）

`pushState` 与 `location.hash` 都被拒。要查：

1. 页面是否被加载在**跨域 iframe** 里（`manifest` 里 `micro_app = true`）。
   验证：`console.log(window.top === window, location.ancestorOrigins)`。
   若在子 frame 里，`pushState` 落在**子 frame 的历史**，而宿主 `canGoBack()` 看的是
   顶层 WebView 历史 —— **这两者不是一回事**，能完美解释"popstate 收到但 canGoBack 为 false"。
2. 换 `location.assign('#mv1g')` 或 `location.replace(url + '#mv1g')` 逐个排除
   "导航 API 被禁"还是"文档不是活跃文档"。

### B. 第二次返回其实发生在聚合根层（面包屑已在最外层）

那就不是"守卫被吃"，而是**退出确认没生效**：

- `syncExitTips()` 依赖 `window.fnosBridge.setExitPageTips({title, content})`。
  2.4.0 曾把参数写成 `{tip: ...}` 导致宿主不弹（已修），但**它在这台设备上是否真的弹过，
  从未被证实**。
- 实验：在根层手动调 `fnosBridge.setExitPageTips({title:'测试',content:'测试'})`，滑一次看是否弹框。
  不弹 ⇒ 该能力不可用，需要在**页面内自绘**根层退出确认（`mvTxAction()` 返回 `'root'` 时触发）。
- 历史教训：自绘确认框必须**跟层级走**，常挂会吞掉"回上一层"（2.1.4 踩过）。

### C. 守卫显示 3 层却仍退出（最坏）

说明宿主 `canGoBack` **不看 WebView session history**，而是它自己的 Flutter 页面栈。
网页侧做任何历史操作都无效。判别：

1. 同一层连滑 3 次，看面板每次是否都有 `popstate HOST`：
   - 都有 ⇒ 宿主在退 WebView 历史，但退到底仍判 false（继续查 §6-A）；
   - 第 2 次没有 `popstate` 就退出 ⇒ 宿主走自己的栈，**网页侧无解**。
2. 若确认无解，唯一出路是**改变交互模型**：手势返回只用于"关大图 / 退一层"，
   在聚合根层由页面自己接管（例如根层时给历史补一条真实 `location.assign` 的导航条目）。

---

## §7 一条尚未被认真尝试的退路

设置里有开关 **「手机端换目录不重载」（`mobileSpaNav`）**，关掉后走 2.4.34 的**真页面导航**，
完全不依赖守卫。但它用 `location.replace`（不压历史），理论上**更容易退出**。

**建议的新实验**（不是简单回退）：把 `navigateTo` 改成 `location.assign`（**压历史**），
并加保护：`history.length` 超过 N（例如 12）时改用 `replace`，
避免"退到根还能退回旧目录"的老问题（2.4.1 踩过）。

这条路的价值：**它不需要我们猜宿主怎么数历史**，用的是宿主最熟悉的语义
（每层一个真 URL）。代价是每次换目录整页加载 —— 但 §1 的目录快照已把首屏做到 352ms，
"真导航 = 每次都闪"这个前提**已经不成立**，值得重新评估。

**它牺牲的是 ②（不重载）**，所以只有在 §6 判定为 A 或 C 且短期无解时才走这条路。

---

## §8 代码地图（只列相关部分）

仓库：`D:/PCAPP/DBAO/mediaview-git`
前端全部在 `src/web/app.js`（约 4700 行，无构建步骤，`go:embed all:web` 嵌入二进制）

### 闪烁 / 重载（§1 前半段）
| 位置 | 作用 |
|---|---|
| `app.js:1555` `layout(keepScroll)` | 重建网格 |
| `app.js:1588` `renderNow()` | **同步**重建（2.4.31：删与建同帧，修桌面档闪烁） |
| `app.js:1583` `requestRender()` | rAF 合并（滚动/缩放等高频路径仍走它） |
| `app.js:918` `SNAP_MAX_AGE` 等 | 目录快照参数（10min / 6 目录 / ≤800 项 / ≤700KB） |
| `app.js:923` `loadSnap()` / `:934` `saveSnap()` / `:959` `sameItems()` | 快照读写与"一致就不重建"判据 |
| `app.js:969` `loadDir(path)` | 先铺快照，再等真实列表 |
| `app.js:4725` `boot()` | `?dir=` 分支不等 settings 先铺快照 |

### 导航 / 返回
| 位置 | 作用 |
|---|---|
| `app.js:852` `mvSyncURL(path)` | `replaceState` 同步 `?dir=`（**不压历史**） |
| `app.js:865` `navigateTo(path)` | 换目录入口；`cfg.mobileSpaNav === false` 时退回真导航 |
| `app.js:893` `navigateRoot()` / `:1054` `goUpLevel()` | 回根层 / 回上一层 |
| `app.js:1144` `MV_GUARD_DEPTH = 3` | 守卫层数 |
| `app.js:1172` `mvOnGuardEntry()` | 判断当前条目是否我们的守卫（认 `state.mvGuard` 或 `#mvNg`） |
| `app.js:1180` `mvPushGuardOnce()` | 压一条守卫：`pushState` + **回读 state**，失败降级 `location.hash` |
| `app.js:1198` `mvGuardArm()` / `:1211` `mvGuardEnsure()` | 补满 / 幂等自愈 |
| `app.js:1228` `mvGuardPoll()` / `:1237` `mvGuardPollStart()` | 前台 400ms 轮询自愈 |
| `app.js:1297` `MV_TX_SETTLE = 180` / `MAX 700` / `TAIL 300` | 返回事务窗口 |
| `app.js:1304` `mvTxAction()` | 这次返回该做什么：`closeViewer` / `upLevel` / `root` |
| `app.js:1311` `mvTxCommit()` | **唯一**执行层级动作处（含幂等校验） |
| `app.js:1334` `mvOnPop()` | `popstate` 唯一入口：记账 → 补守卫 → 归一到事务 |
| `app.js:1437` `mvGuardInit()` | 接线：arm + popstate + pageshow + visibilitychange + 轮询 + touchstart |
| `app.js:2129` `openViewerOverlay()` / `:3669` `closeViewer()` | 查看器自己的一层历史 |
| `src/web/probe_nav.js` | 底部探针面板（**仅排障用**，正式版应删） |

### 诊断口（只读，任意时刻可调）
```js
window.__mvGuardInfo()
// → { depth: 3, viewer: 0, hist: 5, state: {...}, kind: 'state'|'hash'|'',
//     onGuard: true, tx: {a:'upLevel', n:1} | null }
```

---

## §9 构建 / 测试 / 打包 / 装机

```bash
cd D:/PCAPP/DBAO/mediaview-git/src

# 单元测试（全量约 32 秒，必须全绿）
go test -count=1 ./...

# 编译（-buildvcs=false 必须保留，否则 git commit 烙进二进制，重建不可复现）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-s -w" -o ../fpk-x86/app/mediaview .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "-s -w" -o ../fpk-arm/app/mediaview .

# 版本号要改三处：src/main.go 的 version + fpk-x86/manifest + fpk-arm/manifest
# 打包（自算 manifest checksum，mtime 固定 0，同输入同字节）
cd .. && python tools/pack_fpk.py fpk-x86 /tmp/out.fpk
python tools/pack_fpk.py fpk-arm /tmp/out_arm.fpk
```

校验清单：
- magic `1f8b08`、外层条目 **19** 个、`manifest.version` 与二进制内一致
- `manifest.checksum == md5(app.tgz)`
- ELF：x86_64 = `3e00`，AArch64 = `b700`（`binary[18:20].hex()`）
- 二进制里搜得到 `MV_GUARD_DEPTH = N`（确认嵌入新代码）

HTTP 冒烟（Windows 可直接跑）：
```bash
go build -buildvcs=false -o /tmp/mv.exe .
/tmp/mv.exe --port 18101 --workdir /tmp/mvwork &
curl -s --noproxy '*' -o idx.html -w "%{http_code}\n" "http://127.0.0.1:18101/app/mediaview/?dir=/"
```
两个坑：应用挂在 **`/app/mediaview/`** 前缀下；本机有系统代理，`curl` 必须加 `--noproxy '*'`。

测试约定：`zzz_<版本>_<主题>_test.go`，用 `stripLineComments` + `segmentBetween` 对**源码文本**
做静态闸门，再用 node 把真实函数抠出来在模拟环境里跑。
`zzz_2437_return_guard_test.go` 的 `guardSimJS` 能在 `pushOK:false` 下模拟
"pushState 静默失效"的宿主，是验证历史栈方案的必要工具。

---

## §10 红线（改了会引出别的问题）

1. **我们永不主动 `history.back()`**，唯一例外是 `closeViewer()` 收自己压的查看器条目
   （`mvSelfBack` 标记）。主动退历史会与宿主那次叠加。
2. **`replaceState` 只允许出现在 `mvSyncURL` 里**（`zzz_2210` 会数次数）。
3. **不要在 `singleMode`（URL 带 `?path=` 的独立看图窗口）里压守卫**。
4. **守卫只在触屏档启用**（`PROFILE === 'touch'`），桌面档不动。
5. `flow.js`（竖图流）、快照、封面跨会话记忆都与本题无关，改返回时不要顺手碰。
6. **不要删目录快照** —— 它是"真导航退路"唯一的成本补偿。

---

## §11 我的判断（供参考，不要求认同）

优先怀疑 **A 与 B 的组合**：

- 守卫确实没压上（所以加层数无效）；最可能是**页面在跨域 iframe 里**，
  `pushState` 落进子 frame 历史，宿主的 `canGoBack()` 数的是顶层；
- 而用户测试时很可能就在**第 1 层**，"第二次返回"本就发生在聚合根，
  此时即便守卫正常，也该由退出确认接管 —— 而退出确认是否真弹过，从未被证实。

两者叠加，就会稳定复现"第一次正常、第二次退出"，且任何"加层数"的改动都毫无效果。

**建议顺序**：① 取证（§3）区分 A/B/C；② 若 A：查 iframe / 换 `location.assign` 真导航（§7）；
③ 若 B：证实 `setExitPageTips`，不可用就页面内自绘根层确认；④ 若 C：放弃历史层，改交互模型。

---

## §12 附件与位置

| 内容 | 路径 |
|---|---|
| 源码仓库 | `D:/PCAPP/DBAO/mediaview-git`（HEAD `c980a61`） |
| 完整源码包 | `D:/PCAPP/buudy/2026-11-04-22-57-34/deliver-2.4.37/mediaview_2.4.37_src.zip`（同目录 `Y:\mediaview_2.4.37_src.zip`） |
| 安装包 | `deliver-2.4.37/mediaview_2.4.37_{x86,arm}.fpk`、`Y:\mediaview_2.4.37_x86.fpk`（md5 `f6d7b92780a160cfd9744f57ec2d5230`） |
| 交付说明（面板读数含义） | `deliver-2.4.37/mediaview_2.4.37-说明.md` |
| 飞牛 APK（宿主） | `Y:\base.apk` |
| 用户反馈截图 | `Y:\IMG_20261107_*.jpg`、`Y:\Screenshot_2026-11-07-23-05-59-46_*.jpg` |
| 每版完整推理 | 仓库 `CHANGELOG.md`，v2.4.31 / 2.4.34 / 2.4.35 / 2.4.36 / 2.4.37 五条 |
