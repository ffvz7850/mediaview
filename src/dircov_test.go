package main

// 文件夹封面（/api/dircov）的测试。
//
// 分三层：
//   ① 选图（pickCover）—— 只需要"扩展名正确 + 文件存在"的空文件夹具，不碰 ffmpeg；
//   ② 纯函数（parseHexColor / drawCoverFit）—— 不依赖任何全局；
//   ③ 合成（composeCover）—— 端到端，需要 ffmpeg（没有则 skip）。
//
// 铁律：每一条断言都必须能被"变异"变红（见文末变异清单注释）。

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// touch 建一个空文件（选图只认扩展名 + 存在，不读内容）。
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setCoverSeed 固定挑选随机种子（可复现），用完自动还原。
func setCoverSeed(t *testing.T, seed int64) {
	t.Helper()
	coverRandSeed = seed
	t.Cleanup(func() { coverRandSeed = 0 })
}

func paths(items []coverItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Path
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------- ① 选图 ----------

func TestCoverPickDirect(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg"} {
		touch(t, filepath.Join(dir, n))
	}
	p := pickCover(dir, 4)
	if p.Source != "direct" {
		t.Errorf("本层 5 张图应 source=direct，got %q", p.Source)
	}
	if len(p.Items) != 4 {
		t.Fatalf("应取 4 张，got %d", len(p.Items))
	}
	if p.Walked != 5 {
		t.Errorf("本层 5 项应 Walked==5，got %d", p.Walked)
	}
}

func TestCoverPickDeep(t *testing.T) {
	dir := t.TempDir()
	img1 := filepath.Join(dir, "本层1.jpg")
	img2 := filepath.Join(dir, "本层2.jpg")
	touch(t, img1)
	touch(t, img2)
	sub := filepath.Join(dir, "子目录")
	for _, n := range []string{"s1.jpg", "s2.jpg", "s3.jpg", "s4.jpg", "s5.jpg"} {
		touch(t, filepath.Join(sub, n))
	}
	p := pickCover(dir, 4)
	if p.Source != "deep" {
		t.Errorf("本层不足需下探，应 source=deep，got %q", p.Source)
	}
	if len(p.Items) != 4 {
		t.Fatalf("应凑齐 4 张，got %d", len(p.Items))
	}
	// 本层的 2 张必须先算数（图片优先 + 本层优先）
	got := paths(p.Items)
	if !contains(got, img1) || !contains(got, img2) {
		t.Errorf("结果必须含本层的两张，got %v", got)
	}
}

func TestCoverPickNone(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "README.txt"))
	p := pickCover(dir, 4)
	if p.Source != "none" {
		t.Errorf("无媒体文件应 source=none，got %q", p.Source)
	}
	if len(p.Items) != 0 {
		t.Errorf("无图应 0 张，got %d", len(p.Items))
	}
}

func TestCoverPickVideoFallback(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "v.mp4"))
	p := pickCover(dir, 4)
	if len(p.Items) != 1 || p.Items[0].Kind != "video" {
		t.Errorf("纯视频目录应退用视频帧，got %+v", p.Items)
	}
	// 图片优先：目录里既有图又有视频，必须选图
	mix := t.TempDir()
	touch(t, filepath.Join(mix, "v.mp4"))
	touch(t, filepath.Join(mix, "i.jpg"))
	p2 := pickCover(mix, 4)
	if len(p2.Items) != 1 || p2.Items[0].Kind != "image" {
		t.Errorf("图片优先：有图就不该用视频帧，got %+v", p2.Items)
	}
}

func TestCoverPickDepthLimit(t *testing.T) {
	// 2.4.27 起 coverMaxDepth=4：dir → d1..d4，图在 d4（第 4 层）应能抽到；
	// 边界外移到 d5 —— level5 的子目录不入队，图在 d5 取不到。
	dir := t.TempDir()
	deep := filepath.Join(dir, "d1", "d2", "d3", "d4")
	touch(t, filepath.Join(deep, "img.jpg"))
	p := pickCover(dir, 4)
	if len(p.Items) != 1 {
		t.Errorf("maxDepth=4：第 4 层的图应被抽到，got %v", paths(p.Items))
	}
	// 第 5 层超出上限，取不到
	dir2 := t.TempDir()
	tooDeep := filepath.Join(dir2, "d1", "d2", "d3", "d4", "d5")
	touch(t, filepath.Join(tooDeep, "img.jpg"))
	p2 := pickCover(dir2, 4)
	if len(p2.Items) != 0 {
		t.Errorf("超出 maxDepth 的图不应被抽到，got %v", paths(p2.Items))
	}
	if p2.Source != "none" {
		t.Errorf("应 source=none，got %q", p2.Source)
	}
}

func TestCoverPickSkipThumbRoot(t *testing.T) {
	dir := t.TempDir()
	// thumbRoot 指向媒体目录下的 mythumbs（不是 . 开头、也不在 coverSkipDirs 里，
	// 只有靠 thumbRoot 匹配才跳得过）—— 验证"抽到缩略图的缩略图"这一坑被堵住。
	thumbRootMu.Lock()
	thumbRoot = filepath.Join(dir, "mythumbs")
	thumbRootMu.Unlock()
	t.Cleanup(func() {
		thumbRootMu.Lock()
		thumbRoot = ""
		thumbRootMu.Unlock()
	})
	touch(t, filepath.Join(dir, "mythumbs", "a.jpg"))
	p := pickCover(dir, 4)
	if len(p.Items) != 0 {
		t.Errorf("thumbRoot 目录应被跳过，got %v", paths(p.Items))
	}
}

func TestCoverPickSeedReproducible(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg", "f.jpg", "g.jpg"} {
		touch(t, filepath.Join(dir, n))
	}
	setCoverSeed(t, 42)
	p1 := pickCover(dir, 4)
	p2 := pickCover(dir, 4)
	if len(paths(p1.Items)) != 4 || len(paths(p2.Items)) != 4 {
		t.Fatalf("seed 固定应稳定取 4 张")
	}
	a, b := paths(p1.Items), paths(p2.Items)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("固定 seed 两次结果应一致，got %v vs %v", a, b)
		}
	}
}

func TestCoverPickEarlyStop(t *testing.T) {
	// 本层无图，30 个子目录各 1 张图。收集够图就该停，不必展开全部 30 个。
	dir := t.TempDir()
	for i := 0; i < 30; i++ {
		touch(t, filepath.Join(dir, "sub"+string(rune('a'+i%26))+string(rune('a'+i/26)), "x.jpg"))
	}
	setCoverSeed(t, 7)
	p := pickCover(dir, 4)
	if len(p.Items) != 4 {
		t.Fatalf("应取 4 张，got %d", len(p.Items))
	}
	if p.Source != "deep" {
		t.Errorf("应 source=deep，got %q", p.Source)
	}
	// 收集够（16 张）就该 break：Walked 远小于 30 个目录全展开（30 + 30 张图的条目）
	if p.Walked >= 60 {
		t.Errorf("收集够就停应显著少扫，Walked=%d", p.Walked)
	}
}

// TestCoverPickSpreadAcrossSubdirs 用户明确要求的聚合规则：
// 「目录里套很多子文件夹时，随机选四个文件夹、每个取一张」。
// 10 个子目录各 5 张图 → 4 张封面必须来自 4 个**不同**子目录；
// 旧实现是平铺后随机抽，4 张可能全来自同一个子目录（这条会红）。
func TestCoverPickSpreadAcrossSubdirs(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		sub := filepath.Join(dir, "sub"+string(rune('a'+i)))
		for j := 0; j < 5; j++ {
			touch(t, filepath.Join(sub, "img"+string(rune('0'+j))+".jpg"))
		}
	}
	// ⚠️ 必须跑多组种子：单个种子下「平铺后随机抽」也可能凑巧抽到 4 个不同目录，
	//   那就是假通过（实测种子 11 正是这种情况）。这里要求**每一个种子**都满足，
	//   平铺随机在 50 张里抽 4 张全不同目录的概率约 22%，30 个种子全部凑巧的概率可忽略。
	for seed := int64(1); seed <= 30; seed++ {
		setCoverSeed(t, seed)
		p := pickCover(dir, 4)
		if len(p.Items) != 4 {
			t.Fatalf("seed=%d 应取 4 张，got %d", seed, len(p.Items))
		}
		seen := map[string]bool{}
		for _, it := range p.Items {
			seen[filepath.Dir(it.Path)] = true
		}
		if len(seen) != 4 {
			t.Fatalf("★ seed=%d 的 4 张应来自 4 个不同子目录（每个目录各取一张），实际只来自 %d 个: %v",
				seed, len(seen), seen)
		}
	}
}

// TestCoverPickSpreadFewSubdirs 子目录不够 4 个时：先每目录 1 张，再轮转补满，
// 而不是"凑不够就只出 1~2 张"（那样封面会空着大半）。
func TestCoverPickSpreadFewSubdirs(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		sub := filepath.Join(dir, "sub"+string(rune('a'+i)))
		for j := 0; j < 5; j++ {
			touch(t, filepath.Join(sub, "img"+string(rune('0'+j))+".jpg"))
		}
	}
	setCoverSeed(t, 13)
	p := pickCover(dir, 4)
	if len(p.Items) != 4 {
		t.Fatalf("2 个有图子目录也应补满 4 张，got %d", len(p.Items))
	}
	seen := map[string]bool{}
	for _, it := range p.Items {
		seen[filepath.Dir(it.Path)] = true
	}
	if len(seen) != 2 {
		t.Errorf("只有 2 个有图子目录 → 应是 2 个目录各 2 张，实际来自 %d 个", len(seen))
	}
}

// ---------- ② 纯函数 ----------

func TestDrawCoverFit(t *testing.T) {
	// 200x100 纯红 → 100x100 目标区域：cover 裁切取中间 100x100，缩放后仍是红。
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	red := color.RGBA{200, 20, 20, 255}
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			src.Set(x, y, red)
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, 100, 100))
	drawCoverFit(dst, src, image.Rect(0, 0, 100, 100))
	c := dst.RGBAAt(50, 50)
	if c.R < 180 || c.G > 60 || c.B > 60 {
		t.Errorf("cover 缩放后中心应仍是红色，got %v", c)
	}
}

// ---------- ③ 合成 ----------

// solidImg 生成一张纯色图（合成测试直接喂 image.Image，不落盘、不依赖 ffmpeg）。
func solidImg(c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestComposeFromImages(t *testing.T) {
	red := color.RGBA{200, 20, 20, 255}
	green := color.RGBA{20, 180, 20, 255}
	blue := color.RGBA{20, 20, 200, 255}
	white := color.RGBA{240, 240, 240, 255}
	data, err := composeFromImages([]image.Image{solidImg(red), solidImg(green), solidImg(blue), solidImg(white)}, 320)
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("产物不是合法图片: %v", err)
	}
	if cfg.Width != 320 || cfg.Height != 320 {
		t.Errorf("产物尺寸 %dx%d 应 320x320", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	check := func(x, y int, c color.RGBA, name string) {
		got := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
		if diff(got, c) > 40 {
			t.Errorf("%s 象限 (%d,%d) 颜色 %v 偏离 %v", name, x, y, got, c)
		}
	}
	check(80, 80, red, "左上")
	check(240, 80, green, "右上")
	check(80, 240, blue, "左下")
	check(240, 240, white, "右下")
}

func TestComposeFromImagesFew(t *testing.T) {
	// 不足 4 张按实际张数排版：1 铺满 / 2 左右分栏 / 3 左大右二
	red := color.RGBA{200, 20, 20, 255}
	green := color.RGBA{20, 180, 20, 255}
	blue := color.RGBA{20, 20, 200, 255}
	// 1 张铺满
	d1, err := composeFromImages([]image.Image{solidImg(red)}, 320)
	if err != nil {
		t.Fatal(err)
	}
	img1, _, _ := image.Decode(bytes.NewReader(d1))
	if got := color.RGBAModel.Convert(img1.At(160, 160)).(color.RGBA); diff(got, red) > 40 {
		t.Errorf("1 张应铺满，中心颜色 %v 应红", got)
	}
	// 2 张左右分栏：左半红、右半蓝
	d2, err := composeFromImages([]image.Image{solidImg(red), solidImg(blue)}, 320)
	if err != nil {
		t.Fatal(err)
	}
	img2, _, _ := image.Decode(bytes.NewReader(d2))
	if got := color.RGBAModel.Convert(img2.At(80, 160)).(color.RGBA); diff(got, red) > 40 {
		t.Errorf("2 张左半应红，got %v", got)
	}
	if got := color.RGBAModel.Convert(img2.At(240, 160)).(color.RGBA); diff(got, blue) > 40 {
		t.Errorf("2 张右半应蓝，got %v", got)
	}
	// 3 张左大右二：左大第 1 张、右上第 2 张、右下第 3 张
	d3, err := composeFromImages([]image.Image{solidImg(red), solidImg(green), solidImg(blue)}, 320)
	if err != nil {
		t.Fatal(err)
	}
	img3, _, _ := image.Decode(bytes.NewReader(d3))
	if got := color.RGBAModel.Convert(img3.At(80, 160)).(color.RGBA); diff(got, red) > 40 {
		t.Errorf("3 张左大区应红，got %v", got)
	}
	if got := color.RGBAModel.Convert(img3.At(240, 80)).(color.RGBA); diff(got, green) > 40 {
		t.Errorf("3 张右上应绿，got %v", got)
	}
	if got := color.RGBAModel.Convert(img3.At(240, 240)).(color.RGBA); diff(got, blue) > 40 {
		t.Errorf("3 张右下应蓝，got %v", got)
	}
}

func diff(a, b color.RGBA) int {
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	return abs(int(a.R)-int(b.R)) + abs(int(a.G)-int(b.G)) + abs(int(a.B)-int(b.B))
}

// ---------- ⑤ 一键生成 ----------

func TestCollectDirsWithMedia(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a.jpg"))            // 根目录有媒体
	touch(t, filepath.Join(root, "sub", "b.jpg"))     // 子目录有媒体
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)  // 无媒体（只有空目录）
	touch(t, filepath.Join(root, ".hidden", "c.jpg")) // 隐藏目录：跳过
	touch(t, filepath.Join(root, "@eaDir", "d.jpg"))  // 系统目录：跳过

	got, truncated := collectDirsWithMedia([]string{root}, 100, make(chan struct{}))
	if truncated {
		t.Error("100 个额度下不该报截断（truncated 只表示「撞到安全阀、还有目录没展开」）")
	}
	set := map[string]bool{}
	for _, d := range got {
		set[d] = true
	}
	if !set[root] {
		t.Error("根目录有媒体，应被收集")
	}
	if !set[filepath.Join(root, "sub")] {
		t.Error("子目录有媒体，应被收集")
	}
	if set[filepath.Join(root, "empty")] {
		t.Error("无媒体的目录不该被收集")
	}
	if set[filepath.Join(root, ".hidden")] {
		t.Error("隐藏目录不该被收集（否则会把 .mediaview-thumbs 之类也扫进去）")
	}
	if set[filepath.Join(root, "@eaDir")] {
		t.Error("@eaDir 不该被收集")
	}
}

func TestCollectDirsWithMediaMax(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		touch(t, filepath.Join(root, "s"+string(rune('a'+i)), "x.jpg"))
	}
	got, truncated := collectDirsWithMedia([]string{root}, 2, make(chan struct{}))
	if len(got) != 2 {
		t.Errorf("max=2 应只收 2 个，got %d: %v", len(got), got)
	}
	// 5 个子目录只收了 2 个 ⇒ 必须明确报「没走完」。
	// 2.4.45 之前这里是静默 return，界面上完全看不出来只做了一部分。
	if !truncated {
		t.Error("★★ 撞到上限时必须报 truncated —— 否则前端显示「已完成」，用户以为全部做完了")
	}
}

func TestCollectDirsWithMediaCancel(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a.jpg"))
	ch := make(chan struct{})
	close(ch) // 已取消
	if got, _ := collectDirsWithMedia([]string{root}, 100, ch); len(got) != 0 {
		t.Errorf("取消后应立即返回空，got %d", len(got))
	}
}

func waitPregenIdle(t *testing.T) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if !coverPregen.snapshot().Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("一键生成未在 2s 内结束")
}

func TestCoverPregenStartConflictAndCancel(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "a.jpg"))
	t.Cleanup(func() {
		coverPregen.cancelRun()
		waitPregenIdle(t)
	})
	if err := coverPregen.start([]string{dir}, 320, 5); err != nil {
		t.Fatalf("首次启动应成功: %v", err)
	}
	// 重复启动必须被拒（否则两个遍历同时跑会把磁盘打满）
	if err := coverPregen.start([]string{dir}, 320, 5); err == nil {
		t.Error("★ 已在生成中时重复启动必须报错")
	}
	coverPregen.cancelRun()
	waitPregenIdle(t)
	if coverPregen.snapshot().Running {
		t.Error("取消后应停止")
	}
}

// TestCoverPregenSkipIsIdempotent 一键生成必须**幂等**：已生成的目录直接跳过。
//
// 这条是一键生成敢让人反复点的前提 —— 否则每点一次就把整棵树重做一遍，
// NAS 磁盘会被打穿，用户也会以为"按钮没用、一直在跑"。
func TestCoverPregenSkipIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.jpg")
	touch(t, img)
	// 缓存产物放**另一个目录**（真实布局就是这样：缓存在缩略图目录，不在媒体目录里）。
	// 写进 dir 会改掉 dir 自己的 mtime，让下面的「有效性」判定自己失效 —— 那是测试自伤。
	cacheDir := t.TempDir()
	jpg := filepath.Join(cacheDir, "cov.jpg")
	meta := filepath.Join(cacheDir, "cov.json")

	// ① 没有产物 → 不能跳过
	if coverPregenAlreadyDone(dir, jpg, meta) {
		t.Error("★ 产物不存在时不得跳过（那是还没生成）")
	}

	// ② 有产物但缺选图元数据 → 不能跳过
	//   （无法判断这批封面用的是哪几张图、哪个 size，端出去可能是错的）
	if err := os.WriteFile(jpg, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if coverPregenAlreadyDone(dir, jpg, meta) {
		t.Error("★ 缺选图元数据时不得跳过 —— 否则改了 size/bg 后会一直端出旧封面")
	}

	// ③ **元数据有效但产物缺失** → 不能跳过
	//   （上次合成失败 / 有人删了 jpg：这时跳过就等于"永远没有封面"）
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &coverPick{
		V:      coverCacheVersion,
		Dir:    dir,
		PickAt: time.Now().UnixMilli(),
		Items:  []coverItem{{Path: img, Mtime: time.Now().UnixMilli(), Kind: "image"}},
		Checks: []coverCheck{{Path: dir, Mtime: st.ModTime().UnixMilli()}},
	}
	saveCoverPick(meta, p)
	if err := os.Remove(jpg); err != nil {
		t.Fatal(err)
	}
	if coverPregenAlreadyDone(dir, jpg, meta) {
		t.Error("★ 元数据在但产物没了不得跳过 —— 跳过就等于这个目录永远没有封面")
	}

	// ④ 产物 + 有效元数据 → 跳过
	if err := os.WriteFile(jpg, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !coverPregenAlreadyDone(dir, jpg, meta) {
		t.Error("★★ 已生成且元数据有效的目录必须跳过 —— 不跳就等于每点一次重做整棵树")
	}

	// ⑤ 目录内容变了（mtime 变）→ 元数据失效 → 不能跳过（要重抽）
	// 清掉校验结论的 TTL 缓存：TTL 只是让"重抽"最多晚几秒发生，这里要测的是真实校验本身。
	coverValidCache.Delete(jpg)
	if err := os.Chtimes(dir, time.Now().Add(3*time.Second), time.Now().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if coverPregenAlreadyDone(dir, jpg, meta) {
		t.Error("★ 目录内容变化后不得跳过（封面要按新内容重抽）")
	}

	// ⑥ 元数据里没有图（空目录）→ 不能跳过
	p.Items = nil
	saveCoverPick(meta, p)
	if coverPregenAlreadyDone(dir, jpg, meta) {
		t.Error("选图结果为空时不得跳过")
	}
}

// TestCoverPregenGenGuardSurvivesRestart 「取消后立刻重新开始」不能被上一轮任务的状态覆盖。
//
// 取消是协作式的：旧 goroutine 要走到下一个检查点才退出，这段空档里它的收尾会执行到
// defer。没有运行代号守卫时，它会把**新一轮**的 running 置为 false、finished 写成旧时间
// —— 真机冒烟实测表现是「刚点开始，进度瞬间归零、显示已完成」。
func TestCoverPregenGenGuardSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "a.jpg"))
	t.Cleanup(func() {
		coverPregen.cancelRun()
		waitPregenIdle(t)
	})

	if err := coverPregen.start([]string{dir}, 320, 5); err != nil {
		t.Fatalf("首次启动应成功: %v", err)
	}
	coverPregen.cancelRun()
	// ★ 取消后必须能**立刻**重新开始（不必等旧 goroutine 走到检查点）
	if err := coverPregen.start([]string{dir}, 320, 5); err != nil {
		t.Fatalf("★ 取消后立刻重新开始应成功，实际: %v", err)
	}

	coverPregen.mu.Lock()
	cur := coverPregen.gen
	coverPregen.mu.Unlock()

	// 模拟"上一轮任务的收尾"：代号已过期，一个字都不许改
	coverPregen.update(cur-1, func() {
		coverPregen.running = false
		coverPregen.finished = 123
		coverPregen.current = "stale"
	})
	s := coverPregen.snapshot()
	if !s.Running {
		t.Error("★★ 过期代号的任务不得把当前任务标记成已完成（否则点开始后进度瞬间归零）")
	}
	if s.Finished == 123 {
		t.Error("★ 过期代号的任务不得改写 finished")
	}

	// 当前代号仍然可以正常收尾
	coverPregen.update(cur, func() { coverPregen.running = false })
	if coverPregen.snapshot().Running {
		t.Error("当前代号应能正常收尾")
	}
}

// TestCoverSkipRootsIgnoreScanRootInside 媒体目录**本身就在应用工作目录里**时，
// 不能把它的子目录整片跳过。
//
// 这是本机冒烟抓到的真 bug：`workDir` 是应用自己的工作目录，原来的跳过规则是
// "子目录在该根之下就跳过"。只要用户把图库放在应用目录里（bind mount、测试目录都会），
// 每个子目录都满足这条 ⇒ dirs=0、封面永远不出来，而且**没有任何报错**。
func TestCoverSkipRootsIgnoreScanRootInside(t *testing.T) {
	root := t.TempDir()
	oldWork := workDir
	workDir = root
	t.Cleanup(func() { workDir = oldWork })

	media := filepath.Join(root, "library")
	sub := filepath.Join(media, "album")
	touch(t, filepath.Join(sub, "a.jpg"))

	// ① 从工作目录内部扫描：必须能看到子目录
	dirs, _ := collectDirsWithMedia([]string{media}, 100, make(chan struct{}))
	if len(dirs) != 1 || dirs[0] != sub {
		t.Fatalf("★ 扫描根在工作目录里时仍应找到子目录，got %v", dirs)
	}
	setCoverSeed(t, 3)
	if p := pickCover(media, 4); len(p.Items) != 1 {
		t.Fatalf("★ 应能抽到 1 张（不能因为 workDir 是祖先就把整棵树跳过），got %d", len(p.Items))
	}

	// ② 反过来：从**工作目录外面**扫描时，仍然必须避开它
	//    （否则会把应用自己的文件当成媒体素材）
	outside := t.TempDir()
	if !shouldSkipCoverDir("library", media, coverSkipRootsFor(outside)) {
		t.Error("★ 从外部扫描时必须跳过工作目录内的子目录 —— 否则会抽到应用自己的文件")
	}
	// ③ 扫描根本身等于工作目录：也不该因此跳过它的子目录
	if shouldSkipCoverDir("library", media, coverSkipRootsFor(root)) {
		t.Error("扫描根就是工作目录时，不应跳过其子目录")
	}
}

// TestComposeRejectsAllFailedTiles 四块缩略图全失败时**必须报错**，不能返回纯底色图。
//
// 本机冒烟抓到的：缩略图管线不可用（ffmpeg 缺失）时，合成"成功"了但画布只有缝色，
// 这张空色块会被落盘并长期缓存 —— 用户看到一块空灰，比 📁 更糟，而且不会自愈。
func TestComposeRejectsAllFailedTiles(t *testing.T) {
	if _, err := composeFromImages([]image.Image{nil, nil, nil, nil}, 320); !errors.Is(err, errCoverNoTiles) {
		t.Fatalf("★ 全部缩略图失败时必须返回 errCoverNoTiles，实际 err=%v", err)
	}

	// 只要有一块画上就要出图（其余留底色），不能因为个别失败就整张不给
	one := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 40; x++ {
			one.Set(x, y, color.RGBA{200, 10, 10, 255})
		}
	}
	data, err := composeFromImages([]image.Image{nil, one}, 320)
	if err != nil {
		t.Fatalf("有一块成功就应出图，实际 err=%v", err)
	}
	if len(data) == 0 {
		t.Error("产物不应为空")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("产物不是合法图片: %v", err)
	}
}

// ---------- ⑥ 缓存失效 ----------

func TestCoverStillValid(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.jpg")
	touch(t, img)
	st, _ := os.Stat(dir)
	p := &coverPick{
		V:      coverCacheVersion,
		Dir:    dir,
		PickAt: time.Now().UnixMilli(),
		Items:  []coverItem{{Path: img, Mtime: time.Now().UnixMilli(), Kind: "image"}},
		Checks: []coverCheck{{Path: dir, Mtime: st.ModTime().UnixMilli()}},
	}
	if !coverStillValid(p, dir) {
		t.Fatal("内容未变时封面应仍有效")
	}
	// 目录内容变化 → 失效（mtime 变）
	if err := os.Chtimes(dir, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if coverStillValid(p, dir) {
		t.Error("目录 mtime 变化后封面应失效（重抽）")
	}
}
