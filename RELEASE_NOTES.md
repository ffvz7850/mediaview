# mediaview 2.4.51 发布说明

> 极速媒体浏览 —— 飞牛 fnOS 图片/视频极速浏览 FPK 应用
> 仓库：https://github.com/ffvz7850/mediaview
> 版本：2.4.51（x86 / ARM 双架构）

---

## 一句话

修复一个历史一直存在、用户可感知的封面问题：**文件夹封面已全部预生成，但"一键生成后立刻进入路径"仍会重新合成一份**；等十几秒再进就正常。
2.4.51 从根因上修掉这个时序 bug，并补上行为级回归测试把它钉死。

---

## 问题与修复

### 现象

封面明明都生成好了，进入目录时却又重新生成一份；等一小会儿再进就正常。

### 根因

系统会把"这张封面有效/无效"的结论在内存里缓存 8 秒（`coverValidTTL`）。
以前浏览时若某目录暂时没封面，就记下一条"无效"。等"一键生成"把封面写好、新元数据也落盘后，
**写入端没有清掉这条旧的"无效"结论**——你紧接着进目录，读到的还是 8 秒内的旧判断，
于是把刚生成的封面判为失效、重新合成一份。等 8 秒缓存过期再进就恢复正常。

这正好解释了"仅刚生成后短时间会重做、等一会就好"的时序特征。

### 修法（`dircov.go` `saveCoverPick`）

写入端每次写完封面元数据，立即作废这条 jpg 对应的旧有效性结论：

```go
coverValidCache.Delete(coverJPGOf(metaPath))
```

- 放在函数出口统一删，xattr 成功分支与 `.json` 回退分支都覆盖；
- key 用 `coverJPGOf(metaPath)`，与读取端 `loadCoverPick → coverStillValidCached` 完全同口径，不会清错；
- 不改封面元数据结构、不改挑选规则，因此**不需要重建已有封面缓存**（不 bump `coverCacheVersion`，避免十万目录级全量重抽的 IO 峰值）。

---

## 回归测试

新增 `src/zzz_2451_cover_regen_test.go`（3 条）：

| 测试 | 作用 |
|---|---|
| `SaveCoverPickInvalidatesStaleValidity` | 端到端：塞 stale=false → 写 meta → 断言旧结论被清 + 立刻读取命中 |
| `StaleFalseWouldBreakBrowseWithoutInvalidation` | 反向语义：证明 stale=false 确实会误判失效（bug 真实存在） |
| `DeleteCoversBothWritePaths` | 静态闸门：确认 Delete 覆盖 xattr 分支、key 同口径 |

**变异验证**：把修复（那行 `Delete`）去掉后，端到端测试与静态闸门精确变红，证明测试真能抓住回归、不是死断言。

原有的 `Test2424ValidityTTLCachesResult`（TTL 函数语义）与 `zzz_2428`（refresh 清缓存）测的是正确行为，保留不动。

---

## 验证

| 项 | 结果 |
|---|---|
| `gofmt -l *.go` | 无差异 |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test`（全量，含 3 条新测试） | 全部通过（11 项环境相关用例按基线跳过：无 ffmpeg、无符号链接权限、Z 盘实拍） |
| 变异验证（去掉修复） | 新测试变红，确认有效 |
| `node --check web/app.js` | 通过 |
| 二进制 | x86 / ARM 均按 `-trimpath -ldflags "-s -w"` 重新构建，版本串为 2.4.51 |

---

## 安装

从 [Releases](https://github.com/ffvz7850/mediaview/releases) 下载对应架构的 `.fpk` 文件，
在飞牛应用中心手动安装（覆盖安装即可，设置与已有缓存保留；本次修复无需清缓存）。

- x86 架构：`mediaview_2.4.51_x86.fpk`
- ARM 架构：`mediaview_2.4.51_arm.fpk`

升级后验收：对已生成封面的目录，点"一键生成"后立刻进去，应不再重做封面。

---

*本说明对应 v2.4.51；完整版本历史见 `CHANGELOG.md`。上一版 2.4.50 的 22 项审查修复已全部包含在内。*
