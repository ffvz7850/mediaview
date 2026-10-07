# mediaview 版本迭代总览（v1.8.137 → v1.8.149）

> 极速媒体浏览 —— 飞牛 fnOS 图片/视频极速浏览 FPK 应用
> 仓库：https://github.com/ffvz7850/mediaview

---

## 核心目标

替代飞牛网页版图片/视频浏览，极致加载速度。解决网页版浏览媒体时加载慢、卡顿的问题。

---

## 版本迭代总览

### v1.8.149 — 大图切换跟手化

**问题根因**：切换时 `doIt`（位移动作）一直在等 `decode()` 完成，导致点击后画面纹丝不动 1.5 秒——这不是"加载慢"，而是"根本没开始动"。

**改动**：
- 位移立即发生，不等 decode（点击即有反馈）
- 每个媒体盒都带转圈指示（原来只有当前图有，切换目标图没有转圈导致空白）
- 删除 3 个纯重复的预取函数 + `preloadCache`，换成 `warmTrackBoxes()`（−45 行）
- `anim-none` 立即收尾，干掉 400ms 死窗
- 修复 `ensureMediaVisible` 被 `if` 误吞的 bug

**效果**：点击切换 → 索引/计数/标题立即更新 → 画面立即朝目标方向动 → 目标位置显示转圈 → 位图就绪后转圈消失、图片出现。既不空白也不闪。

---

### v1.8.148 — 缓存目录精简

**改动**：
- 缩略图内容标识从 `.key` sidecar 文件改存 xattr（`user.mediaview.key`），缓存目录变纯镜像
- `.meta.json` 不再落盘，只留内存 `metaMem`
- 三级回退：xattr → 旧 `.key` sidecar（只读，升级兼容）→ 产物存在即命中
- 修复缓存统计 bug（原来只跳过 `.key` 没跳过 `.meta.json`，统计虚高一倍）

---

### v1.8.147 — 换目录清待办

**改动**：进入新目录时非阻塞清空上个目录遗留的缩略图待办队列，只丢未取走的任务，正在生成的不打断。

---

### v1.8.146 — 设置规格统一 + 预生成开关

**改动**：
- 设置界面规格统一（所有下拉/开关对齐）
- 后台预生成改成开关 + 手动执行按钮（原来默认开 2 个 worker）
- 大图清晰度默认从 4096 改为 2048

---

### v1.8.145 — 修复读取设置失败

**改动**：修复配置文件读取失败时回退默认值的逻辑。

---

### v1.8.142~1.8.144 — 1:1 一次算到位 + 大图设置项

**改动**：
- 1:1 直接取原图（不经过 4096 中间档，换源次数最少=最不闪）
- `ensureItemSize` 预取尺寸 + `_actualScaleLocked` 锁死，不再二次放大
- 新增大图预览设置项：画质（ViewerQuality）、清晰度上限（ViewerMaxdim）、lowres 档位（ViewerLowres）

---

### v1.8.138 — 大图 lowres 提速 + 预取节制

**改动**：
- 大图 lowres 加 `minRatio=0.9`（允许缩到目标的 90%，3072 档快 39%，只缩 5%）
- 预取节制：`PREFETCH_MAX=2`、延后 250ms、去掉第 3 个预取
- 1:1 三跳修复

---

### v1.8.137 — 迁移修复 + auto 统一走 ffmpeg

**改动**：
- 修复 ThumbLowres 迁移缺陷（normalize 漏字段 + schema 4 补迁移）
- auto 引擎在 lowres 开启时统一走 ffmpeg（实测 baseline 快 2.2×）
- 大图质量区间 [88,92] → [82,86]（稳定 -q:v 4）
- `scaledSem` 容量 2 → 3
- 双向预加载

---

## 核心架构

### 缩略图引擎（三路径分流）

| 引擎 | 适用场景 | 特点 |
|---|---|---|
| **ffmpeg** | auto 模式（lowres 开启时统一走） | 支持 -lowres 快速解码，渐进式 JPEG 快 26% |
| **libvips** | auto 模式（lowres 关闭时普通图） | shrink-on-load，普通图快 34% |
| **Go 原生** | 兜底 | 不依赖外部二进制 |

- 视频抽帧走 VAAPI 硬件解码（H.264/HEVC），熔断机制：连续失败 5 次后关闭硬解 10 分钟
- lowres 动态档位：`N = clamp(floor(log2(长边/目标)), 0, 3)`，保证 `长边/2^N ≥ 目标×0.9`

### 大图浏览

- 默认清晰度 2048px，可设置 2048/3072/4096/5120/6144
- lowres 自动档位（可关闭或固定 1/2/3）
- 画质：自动(88) / 跟随缩略图 / 固定 84/88/95
- 翻页动画：slide / fade / zoom / none
- 预加载：默认 2 张（可设置 0~5）

### 系统缩略图接管

- 监听 `/var/run/auto_thumbnailer.socket`，实现飞牛相同 API
- size=list(320) / medium(归一化为 list) / big(1920)
- size=big 触发 `noteForegroundPreview()`：暂停后台 + kill 在跑 ffmpeg
- 大图浏览期间 list/medium 立即返回透明 GIF（不生成），把浏览器连接让给大图

### 前后台资源让路

- 大图浏览时：暂停后台预生成 + kill 在跑 ffmpeg
- 前台请求挂起上限 2.5s
- 预留槽 `thumbImmediateSem` 给大图专用
- 关闭大图 1.5 秒后继续缩略图生成

### 缓存

- 缩略图缓存：用户自定义目录下的 `.mediaview-thumbs/`
- 内容标识：xattr `user.mediaview.key`（含 path+mtime+size）
- 大图缩放缓存：`/tmp/mediaview-scaled/`，每 20 次生成清理一次超过 1 小时的旧文件
- 失败负缓存：10 分钟 TTL，避免 svg/heic/损坏文件反复重跑

---

## 性能数据

| 场景 | 优化前 | 优化后 | 提升 |
|---|---|---|---|
| 普通图 →320 缩略图（lowres） | 387ms | 173ms | 快 55% |
| 渐进式 102MP →320（lowres） | 1452ms | 1035ms | 快 29% |
| 渐进式 102MP →1920（lowres） | 1582ms | 1169ms | 快 26% |
| 大图 3072 档（minRatio=0.9） | 1937ms | 1196ms | 快 39% |
| auto 引擎统一走 ffmpeg | baseline | 2.2× 快 | — |

---

## 安装

从 [Releases](https://github.com/ffvz7850/mediaview/releases) 下载对应架构的 `.fpk` 文件，在飞牛应用中心手动安装。

- x86 架构：`mediaview_x.x.x_x86.fpk`
- ARM 架构：`mediaview_x.x.x_arm.fpk`

---

*最后更新：v1.8.149*
