package main

// 2.4.28 的回归测试：把「之前版本说做了、代码里其实没落实」的几处钉死。
//
//	① 下探走过的**子目录**必须进 Checks（2.4.14 起只记了根目录 ⇒ 往子目录里
//	   加/删照片，封面永远不刷新，只有清缓存能救）；
//	② 「一键生成」必须看有没有真产出（撞预算一张没抽到曾被算成"已生成"）；
//	③ 去重键必须按平台折叠大小写（Linux 上 Photos/photos 是两个目录）；
//	④ refresh=1 必须作废旧的有效性结论（否则下一次普通请求又白抽一遍）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- ① 子目录进 Checks ----------

func Test2428SubdirChecksRecorded(t *testing.T) {
	setCoverSeed(t, 11)

	dir := t.TempDir()
	sub := filepath.Join(dir, "年", "2024")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(sub, "a.jpg"))

	p := pickCover(dir, coverMaxItems)
	if len(p.Items) != 1 {
		t.Fatalf("应抽到子目录里的 1 张，got %d", len(p.Items))
	}
	// 根 + 走过的两层子目录：至少 3 条（根、年、2024）
	if len(p.Checks) < 3 {
		t.Fatalf("★ Checks 只有 %d 条（%v）——下探走过的子目录没记进来", len(p.Checks), p.Checks)
	}
	var hasSub bool
	for _, c := range p.Checks {
		if samePath(c.Path, sub) {
			hasSub = true
		}
	}
	if !hasSub {
		t.Errorf("★ Checks 里必须有实际取图的那个子目录 %s，got %v", sub, p.Checks)
	}

	// 行为面：往子目录里再加一张图（父目录 mtime 不变）⇒ 旧结论必须失效
	if !coverStillValid(&p, dir) {
		t.Fatal("刚挑完就判无效，说明 Checks 里写进了错误路径")
	}
	touch(t, filepath.Join(sub, "b.jpg"))
	if coverStillValid(&p, dir) {
		t.Error("★ 子目录内容变了，封面仍判有效 = 陈旧封面回归（这正是 2.4.28 修的那条）")
	}
}

// ---------- ② 一键生成的结果归类 ----------

func Test2428PregenCount(t *testing.T) {
	// 有错 ⇒ failed
	if g, s, f := pregenCount(nil, errCoverNoTiles); g != 0 || s != 0 || f != 1 {
		t.Errorf("有错应只算 failed，got gen=%d skip=%d fail=%d", g, s, f)
	}
	// 无错但没 Items（撞预算/真空目录）⇒ skipped，绝不能算 generated
	if g, s, f := pregenCount(&coverPick{Items: []coverItem{}}, nil); g != 0 || s != 1 || f != 0 {
		t.Errorf("没有产出应算 skipped，got gen=%d skip=%d fail=%d", g, s, f)
	}
	// 有产出 ⇒ generated
	if g, s, f := pregenCount(&coverPick{Items: []coverItem{{Path: "a.jpg"}}}, nil); g != 1 || s != 0 || f != 0 {
		t.Errorf("有产出应算 generated，got gen=%d skip=%d fail=%d", g, s, f)
	}

	// 静态闸门：runCoverPregen 里不许再出现"err==nil 就 generated++"的老写法
	src := stripLineComments(readSourceOrSkip(t, "dircov.go"))
	if strings.Contains(src, "coverPregen.generated++") {
		t.Error("★ runCoverPregen 不得再无条件 generated++（要看 len(p.Items)）")
	}
	if !strings.Contains(src, "pregenCount(p, err)") {
		t.Error("★ runCoverPregen 必须用 pregenCount 归类")
	}
}

// ---------- ③ 去重键的平台差异 ----------

func Test2428PathKeyPlatform(t *testing.T) {
	a := pathKey("/vol1/1000/Photos")
	b := pathKey("/vol1/1000/photos")
	if runtime.GOOS == "windows" {
		if a != b {
			t.Errorf("Windows 上只差大小写的两个路径应得同一个键，got %q vs %q", a, b)
		}
	} else {
		if a == b {
			t.Errorf("Linux 上 Photos/photos 是两个不同目录，键不能相同（got 都是 %q）", a)
		}
	}
	if pathKey("/vol1/1000/Photos/") != pathKey("/vol1/1000/Photos") {
		t.Error("同一目录的不同写法必须得到同一个键（Clean 未生效）")
	}
	// 静态闸门：本机是 Windows，"无条件 ToLower" 在行为上也能过 —— 必须有平台分支才算数。
	// （这正是"断言过但代码没落实"的典型：测试跑在哪个平台，就只能证明那个平台。）
	cfgSrc := readSourceOrSkip(t, "config.go")
	seg := segmentBetween(cfgSrc, "func pathKey(", "\n}")
	if seg == "" {
		t.Fatal("config.go 里找不到 pathKey")
	}
	if !strings.Contains(seg, "runtime.GOOS") {
		t.Error("★ pathKey 必须按平台分支（Windows 折叠、Linux 精确）")
	}
	// 静态闸门：collectDirsWithMedia 里不得再直接 ToLower 当去重键
	src := stripLineComments(readSourceOrSkip(t, "dircov.go"))
	i := strings.Index(src, "func collectDirsWithMedia")
	if i < 0 {
		t.Fatal("找不到 collectDirsWithMedia")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	if strings.Contains(body, "strings.ToLower") {
		t.Error("★ collectDirsWithMedia 不得用 strings.ToLower 去重（Linux 上会漏目录）")
	}
}

// ---------- ⑤ meta 体积闸门（xattr 4KB）----------

// 满配 meta 必须塞得进 xattr：塞不进就退回 .json sidecar，
// 而"目录里多一个 json"正是用户 2.4.23 明确要求去掉的东西。
// 2.4.28 把 coverMaxChecks 从 32 收到 16 就是因为实测 32 条 = 4604B 顶破 4032B。
func Test2428MetaFitsXattr(t *testing.T) {
	p := &coverPick{V: coverCacheVersion, Dir: "/vol1/1000/我的照片", Source: "deep", Depth: 4, Walked: 800}
	for i := 0; i < coverMaxChecks; i++ {
		p.Checks = append(p.Checks, coverCheck{
			Path:  fmt.Sprintf("/vol1/1000/我的照片/2024年/家庭旅行/日本东京/浅草寺与晴空塔/第%d天", i),
			Mtime: 1793976983295,
		})
	}
	for i := 0; i < coverMaxItems; i++ {
		p.Items = append(p.Items, coverItem{
			Path:  fmt.Sprintf("/vol1/1000/我的照片/2024年/家庭旅行/日本东京/浅草寺与晴空塔/第%d天/IMG_%04d.HEIC", i, i),
			Mtime: 1793976983295, Kind: "image",
		})
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > coverMetaXattrLimit {
		t.Errorf("★ 满配 meta %d B 超过 xattr 上限 %d —— 会退回 .json（用户明确不要）", len(raw), coverMetaXattrLimit)
	}
	if coverMaxChecks > 32 {
		t.Errorf("checks 上限放宽到 %d，先重新量一遍 meta 体积再说", coverMaxChecks)
	}
}

// ---------- ④ refresh 必须作废旧的有效性结论 ----------

func Test2428RefreshDropsValidTTL(t *testing.T) {
	root := setupTempRoot(t)
	dir := filepath.Join(workDir, "相册")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(dir, "a.jpg"))

	base := coverCacheBase(dir, coverDefaultSize)
	if base == "" {
		t.Fatalf("没有缩略图根目录（root=%q）", root)
	}
	key := base + ".jpg"
	// 先塞一个"无效"结论进 TTL 缓存，模拟"8 秒前刚判过死"
	coverValidCache.Store(key, coverValidEntry{valid: false, at: time.Now()})
	t.Cleanup(func() { coverValidCache.Delete(key) })

	w := httptest.NewRecorder()
	handleDirCover(w, httptest.NewRequest(http.MethodGet,
		"/api/dircov?path="+dir+"&size="+strconv.Itoa(coverDefaultSize)+"&refresh=1", nil))
	// 本机没有 ffmpeg/图片，结果可能是 404，但"作废 TTL"这件事必须发生
	if _, ok := coverValidCache.Load(key); ok {
		t.Error("★ refresh=1 之后旧的有效性结论必须被删掉（否则下一次普通请求会白抽一遍）")
	}
}
