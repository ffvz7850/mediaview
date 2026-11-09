package main

// 2.4.45：修「一键生成封面最多只有 3000 个文件夹」。
//
// 根因两层，缺一层都不会出现这个症状：
//   ① 后端 coverPregenMaxDirs 是**产品意义上的硬上限**（3000），不是安全阀；
//   ② 前端启动时**没有传 max**，所以这个 3000 一直生效；
//   ③ 撞上限时 collectDirsWithMedia 静默 return —— 进度条走完 3000/3000，
//      界面显示"已完成"，用户根本看不出后面几千个目录没做。
//
// 所以本测试钉三件事：默认值必须足够大且只能当安全阀、撞上限必须上报 truncated、
// 前端必须把 truncated 说出来（不能只显示"已完成"）。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- ① 静态闸门 ----------

func Test2445PregenLimitStatic(t *testing.T) {
	src := stripLineComments(readSourceOrSkip(t, "dircov.go"))

	m := regexp.MustCompile(`const coverPregenMaxDirs = (\d+)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("★ 找不到 coverPregenMaxDirs —— 结构变了，这条闸门失效")
	}
	n, _ := strconv.Atoi(m[1])
	if n < 50000 {
		t.Errorf("★★ coverPregenMaxDirs = %d：它现在的角色是「防病态目录树的安全阀」，不是产品上限。\n"+
			"   历史上它固定 3000 且前端不传 max，于是目录多的时候只做前 3000 个，"+
			"界面还显示「已完成」—— 这就是「一键生成最多只有 3000 个」的来源。", n)
	}

	// URL 的 max 上界必须引用常量：两处各写一个数字迟早分叉（3000 / 20000 就是这么来的）
	if !strings.Contains(src, "m <= coverPregenMaxDirs") {
		t.Error("★★ handler 里 max 的上界必须引用 coverPregenMaxDirs，不许另写字面量 —— " +
			"前端传更大的值会被静默截断，用户完全看不出来")
	}
	if regexp.MustCompile(`m <= \d+`).MatchString(src) {
		t.Error("★ handler 里还留着硬编码的 max 上界数字，请改成 coverPregenMaxDirs")
	}

	// 截断必须传到状态视图（前端才有机会提示）
	if !strings.Contains(src, "Truncated: s.truncated") {
		t.Error("★★ snapshot 里必须带 Truncated —— 否则 /api/dircov/pregen 拿不到「没走完」这个事实")
	}
	if !strings.Contains(src, "coverPregen.truncated = truncated") {
		t.Error("★★ runCoverPregen 必须把 collect 的 truncated 写进状态")
	}
}

// ---------- ② 行为：撞上限必须报 truncated ----------

func Test2445CollectTruncated(t *testing.T) {
	root := t.TempDir()
	const n = 30
	for i := 1; i <= n; i++ {
		touch(t, filepath.Join(root, fmt.Sprintf("d%02d", i), "x.jpg"))
	}

	got, truncated := collectDirsWithMedia([]string{root}, 10, make(chan struct{}))
	if len(got) != 10 {
		t.Errorf("max=10 应只收 10 个，实际 %d", len(got))
	}
	if !truncated {
		t.Error("★★ 30 个目录只收了 10 个，必须报 truncated=true —— " +
			"静默截断正是「界面说已完成、实际只做了一部分」的根因")
	}

	// 额度足够时：收全，且**不许**误报截断（否则用户会一直点「继续」）
	all, allTrunc := collectDirsWithMedia([]string{root}, 1000, make(chan struct{}))
	if len(all) != n {
		t.Errorf("额度足够时应收全 %d 个，实际 %d", n, len(all))
	}
	if allTrunc {
		t.Error("★ 已经走完整棵树还报 truncated —— 前端会一直让用户「再点一次继续」")
	}

	// 恰好收满最后一个、队列也空了 ⇒ 不算截断
	exact, exactTrunc := collectDirsWithMedia([]string{root}, n, make(chan struct{}))
	if len(exact) != n || exactTrunc {
		t.Errorf("恰好收满不叫截断：got %d truncated=%v（应为 %d/false）", len(exact), exactTrunc, n)
	}
}

// ---------- ③ 状态链路：start → 跑完 → snapshot 带 truncated ----------

func Test2445PregenStateReportsTruncated(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 12; i++ {
		touch(t, filepath.Join(dir, fmt.Sprintf("d%02d", i), "x.jpg"))
	}
	t.Cleanup(func() {
		coverPregen.cancelRun()
		waitPregenIdle(t)
		// 复位，别把 truncated 泄漏给后面的测试
		coverPregen.mu.Lock()
		coverPregen.truncated = false
		coverPregen.mu.Unlock()
	})

	if err := coverPregen.start([]string{dir}, coverDefaultSize, 5); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	waitPregenIdle(t)

	v := coverPregen.snapshot()
	if v.Total != 5 {
		t.Errorf("total 应为 5（额度），实际 %d", v.Total)
	}
	if !v.Truncated {
		t.Error("★★ 12 个目录只排了 5 个，snapshot 必须带 truncated=true —— " +
			"前端全靠这个字段决定「要不要告诉用户还有没做完的」")
	}
	if v.Done != v.Total {
		t.Errorf("done=%d 应等于 total=%d", v.Done, v.Total)
	}
}

// ---------- ④ 前端：完成文案必须说出「没走完」 ----------

func Test2445FrontendTruncatedText(t *testing.T) {
	app := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "app.js")))

	// 两处（轮询完 / 重进设置页恢复）必须用同一个文案函数：
	// 各写一份的下场就是改了一处、另一处还在说"已完成"。
	if strings.Count(app, "coverPregenDoneText(s)") < 2 {
		t.Error("★★ coverPregenPoll 与 coverPregenRestore 都必须用 coverPregenDoneText()，" +
			"不许各拼一份 —— 否则两条路径的完成文案会分叉")
	}
	if !strings.Contains(app, "s.truncated") {
		t.Error("★★ 前端必须判 truncated，否则目录没走完时仍然显示「已完成」")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	appPath, _ := filepath.Abs(filepath.Join("web", "app.js"))
	harness := `
const fs = require('fs');
const src = fs.readFileSync(process.env.MV_APPJS, 'utf8');
function grab(name) {
  const i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('no ' + name);
  let d = 0;
  for (let k = src.indexOf('{', i); k < src.length; k++) {
    if (src[k] === '{') d++;
    else if (src[k] === '}') { d--; if (!d) return src.slice(i, k + 1); }
  }
  throw new Error('unbalanced ' + name);
}
const fn = new Function(grab('coverPregenDoneText') + '; return coverPregenDoneText;')();
console.log(JSON.stringify({
  normal: fn({ generated: 120, skipped: 80 }),
  failed: fn({ generated: 1, skipped: 2, failed: 3 }),
  cut:    fn({ generated: 3000, skipped: 0, total: 3000, truncated: true })
}));
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Env = append(os.Environ(), "MV_APPJS="+appPath)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node 运行失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("node 运行失败：%v", err)
	}
	var res map[string]string
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	if !strings.Contains(res["normal"], "新生成 120") || strings.Contains(res["normal"], "再点一次") {
		t.Errorf("正常完成不该出现「再点一次」：%q", res["normal"])
	}
	if !strings.Contains(res["failed"], "失败 3") {
		t.Errorf("失败数必须显示：%q", res["failed"])
	}
	if !strings.Contains(res["cut"], "再点一次") || !strings.Contains(res["cut"], "3000") {
		t.Errorf("★★ 截断时必须告诉用户「再点一次继续」并给出本次处理的数量：%q", res["cut"])
	}
}

// Test2445BeyondOldLimit 直接复现用户场景：目录数超过历史的 3000 个硬上限时，
// 默认额度必须把它们**全部**排进来（这条在 2.4.44 会红：只能收到 3000，且报"./已完成"）。
func Test2445BeyondOldLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("造 3200 个目录较慢，short 模式跳过")
	}
	root := t.TempDir()
	const n = 3200
	for i := 0; i < n; i++ {
		touch(t, filepath.Join(root, fmt.Sprintf("d%05d", i), "x.jpg"))
	}
	got, truncated := collectDirsWithMedia([]string{root}, coverPregenMaxDirs, make(chan struct{}))
	if len(got) != n {
		t.Errorf("★★ 默认额度下应排进全部 %d 个目录，实际 %d —— 「一键生成最多只有 3000 个」又回来了",
			n, len(got))
	}
	if truncated {
		t.Error("★ 没撞到安全阀却报了截断")
	}
}

// Test2445DeepNestingCollected 用户问的"子文件夹套得很深会不会漏"：
// 遍历**没有深度上限**（BFS 走到底），只有"一个封面往下探几层找图"才有 4 层限制。
// 这里造 12 层嵌套、每层都有图，要求 12 层**全部**被收集。
func Test2445DeepNestingCollected(t *testing.T) {
	root := t.TempDir()
	const depth = 12
	cur := root
	var want []string
	for i := 0; i < depth; i++ {
		cur = filepath.Join(cur, fmt.Sprintf("L%02d", i))
		touch(t, filepath.Join(cur, "x.jpg"))
		want = append(want, cur)
	}

	got, truncated := collectDirsWithMedia([]string{root}, coverPregenMaxDirs, make(chan struct{}))
	if truncated {
		t.Fatal("★ 12 层嵌套不该撞安全阀")
	}
	if len(got) != depth {
		t.Fatalf("★★ 12 层嵌套应收集 %d 个目录，实际 %d —— 深层目录被漏掉了：%v",
			depth, len(got), got)
	}
	set := map[string]bool{}
	for _, d := range got {
		set[d] = true
	}
	for _, d := range want {
		if !set[d] {
			t.Errorf("★★ 漏了深层目录 %s", d)
		}
	}
}

// Test2445SymlinkDirNotFollowed 目录软链不跟随：既不会漏真实目录，也绝不会成环死循环。
func Test2445SymlinkDirNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 建符号链接需要管理员/开发者模式")
	}
	root := t.TempDir()
	real := filepath.Join(root, "real", "a")
	touch(t, filepath.Join(real, "x.jpg"))
	// 指回 root 的软链：若被跟随就会无限递归
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Skipf("建软链失败：%v", err)
	}
	done := make(chan struct{})
	var got []string
	go func() {
		got, _ = collectDirsWithMedia([]string{root}, coverPregenMaxDirs, make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("★★ 扫描没在 10s 内结束 —— 很可能跟随了目录软链，成环了")
	}
	if len(got) != 1 || got[0] != real {
		t.Errorf("应只收集真实目录 %s，实际 %v", real, got)
	}
}

// Test2445DeepNestingPickCover 深层目录自己的封面也要能挑到图。
// 遍历（收集哪些目录）没有深度上限；这里钉的是"被收集到的深层目录，挑图能命中本层"。
func Test2445DeepNestingPickCover(t *testing.T) {
	root := t.TempDir()
	cur := root
	for i := 0; i < 6; i++ {
		cur = filepath.Join(cur, fmt.Sprintf("L%02d", i))
	}
	touch(t, filepath.Join(cur, "deep.jpg"))

	p := pickCover(cur, 4)
	if len(p.Items) == 0 {
		t.Fatalf("★★ 第 6 层目录的封面挑不到图（Items=0, source=%s）", p.Source)
	}
	if p.Items[0].Path != filepath.Join(cur, "deep.jpg") {
		t.Errorf("应优先用本层的图，实际 %v", p.Items)
	}
}
