package main

// 2.4.40：端到端证明「进目录 / 返回 / 前进」在真页面导航下**不闪**。
//
// 单元测试证明不了这件事 —— 它只证明"代码是这么写的"。真正的判据是**帧**：
// 内容出现之后还有没有出现过"0 个格子"的帧（那就是用户看到的空白闪），
// 以及"内容第一次出现"用了多久。
//
// 探针（tools/realnav_probe.js）自带假后端，把 src/web 真跑在 headless Edge 里，
// 逐帧采样 #gridContent.children.length，跑完 进目录 → 返回 → 前进 三轮，输出 JSON。
//
// 两条关键设计：
//   · 假后端的 /api/list **故意延迟 600ms**：真机上扫盘就是这么慢。于是"首访 700~900ms、
//     返回 ~60ms"这组数字本身就证明了**快照**在起作用（否则返回也要等 600ms）。
//   · 刻意加 `MV_NO_BFCACHE=1`：飞牛 App 的 WebView 不保证有前进/后退缓存，
//     要量的是**最坏情况** —— 每次返回都真的重建文档、真的重新拉接口。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type realnavDoc struct {
	Doc        int    `json:"doc"`
	Frames     int    `json:"frames"`
	FirstCells *int   `json:"firstCells"`
	BlankAfter int    `json:"blankAfter"`
	MaxCells   int    `json:"maxCells"`
	Href       string `json:"href"`
}

func Test2440BackForwardNoFlashRealBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实浏览器")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	edge := os.Getenv("MV_EDGE")
	if edge == "" && runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				edge = p
				break
			}
		}
	}
	if edge == "" {
		t.Skip("没有 Edge，跳过真实渲染验证")
	}

	cmd := exec.Command(node, filepath.Join("..", "tools", "realnav_probe.js"))
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MV_EDGE="+edge, "MV_NO_BFCACHE=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("探针运行失败：%v", err)
	}
	var res struct {
		Root          realnavDoc `json:"root"`
		Sub           realnavDoc `json:"sub"`
		Back          realnavDoc `json:"back"`
		Forward       realnavDoc `json:"forward"`
		ScrolledAt    int        `json:"scrolledAt"`
		ForwardScroll int        `json:"forwardScroll"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}

	// ① 换目录真的是"换文档"（不是内部切换）—— 否则后面几项验的就不是真导航
	if res.Sub.Doc <= res.Root.Doc {
		t.Fatalf("★★ 点文件夹没有产生新文档（doc %d → %d）：说明又变成内部切换了 —— "+
			"那正是「返回就退出应用」的根源", res.Root.Doc, res.Sub.Doc)
	}
	if res.Back.Doc <= res.Sub.Doc {
		t.Errorf("★★ 返回没有重建文档（doc %d → %d）—— 探针要求 MV_NO_BFCACHE 下的最坏路径",
			res.Sub.Doc, res.Back.Doc)
	}
	if !strings.Contains(res.Back.Href, "?dir=/") {
		t.Errorf("★ 返回后 URL 应是根层（?dir=/），实际 %s", res.Back.Href)
	}

	// ② 任何一处都不许"内容出现后又变回空白"
	for name, d := range map[string]realnavDoc{
		"进目录": res.Sub, "返回": res.Back, "前进": res.Forward,
	} {
		if d.FirstCells == nil {
			t.Errorf("★★ %s 后一直没有格子（%d 帧）—— 页面是空的", name, d.Frames)
			continue
		}
		if d.BlankAfter > 0 {
			t.Errorf("★★ %s 后内容出现又变回 0 格：%d 帧 —— 这就是肉眼看到的那一下闪",
				name, d.BlankAfter)
		}
	}

	// ③ **核心**：返回 / 前进必须在接口回来之前就有画面。
	//    假后端的 /api/list 是 600ms，所以"首访要 700ms+"是正常的；
	//    返回/前进若也接近 600ms，就说明快照根本没起作用（用户看到的就是那一下加载）。
	for name, d := range map[string]realnavDoc{"返回": res.Back, "前进": res.Forward} {
		if d.FirstCells == nil {
			continue
		}
		if *d.FirstCells > 300 {
			t.Errorf("★★ %s 后 %.0fms 才有第一帧内容 —— 而 /api/list 是 600ms，"+
				"说明快照没兜住（这一下就是用户说的「返回要加载」）", name, float64(*d.FirstCells))
		}
	}
	if res.Back.MaxCells < 3 {
		t.Errorf("★ 返回根层只画出了 %d 个格子（应为 3 个文件夹）", res.Back.MaxCells)
	}

	// ④ 回到原处：离开前停在 300，前进回来必须还在 300（快照里的 top 起作用）
	if res.ScrolledAt < 250 {
		t.Fatalf("探针没能把列表滚下去（scrolledAt=%d）", res.ScrolledAt)
	}
	if diff := res.ForwardScroll - res.ScrolledAt; diff > 24 || diff < -24 {
		t.Errorf("★★ 前进回来后滚动位置是 %d，离开时是 %d —— 快照里的 top 没被还原"+
			"（用户会感觉「怎么又跳回顶部了」）", res.ForwardScroll, res.ScrolledAt)
	}

	t.Logf("首帧内容：首次进入 %v ms / 返回 %v ms / 前进 %v ms（list 故意 600ms；blankAfter 全 0）",
		deref(res.Sub.FirstCells), deref(res.Back.FirstCells), deref(res.Forward.FirstCells))
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
