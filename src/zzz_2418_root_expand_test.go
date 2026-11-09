package main

// 2.4.18：首页把「已添加的文件夹」**展开一层**。
//
// 用户反馈：「软件内应该显示文件夹内的子文件夹，而不是单个文件夹当第一层级」。
// 添加的是"图库根"，期望打开就看到里面的分类目录，而不是再点一层；
// 与飞牛相册的观感一致。
//
// 这里守住四条容易出错的边界：
//  ① 有内容 ⇒ 展开（不能同时把 root 自己也列出来，否则等于没展开）；
//  ② 展开后为空 ⇒ **回退显示它自己**（否则用户以为添加的文件夹消失了，也进不去）；
//  ③ 系统目录（#recycle / @eaDir …）不进首页，但点进去仍然看得到；
//  ④ 多个 root 合并后按同一套规则重排（不能是"A 的一组、B 的一组"）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Test2418RootExpandsOneLevel(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "照片库")
	sub1 := filepath.Join(root, "2023")
	sub2 := filepath.Join(root, "2024")
	recycle := filepath.Join(root, "#recycle")
	ea := filepath.Join(root, "@eaDir")
	mkdirs(t, sub1, sub2, recycle, ea)
	// root 下直接放两张图（散图必须也能看到，不能因为"只显示子文件夹"被吞掉）
	touch(t, filepath.Join(root, "cover.jpg"))
	touch(t, filepath.Join(root, "b.mp4"))
	// 系统目录里也放点东西，证明是"按名字跳过"而不是"恰好空"
	touch(t, filepath.Join(recycle, "deleted.jpg"))
	touch(t, filepath.Join(ea, "thumb.jpg"))

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{root}})
	resp := aggregateRoot("name")

	paths := make([]string, 0, len(resp.Dirs))
	for _, d := range resp.Dirs {
		paths = append(paths, d.Path)
	}
	if !containsPath(paths, sub1) || !containsPath(paths, sub2) {
		t.Errorf("★★ 首页必须展开显示 root 里的子文件夹，实际 %v", paths)
	}
	if containsPath(paths, root) {
		t.Error("★★ 展开后不能再把 root 自己当成一个格子（那就等于没展开，用户还要再点一层）")
	}
	if containsPath(paths, recycle) || containsPath(paths, ea) {
		t.Errorf("系统目录不该出现在首页（回收站/群晖缩略图目录），实际 %v", paths)
	}

	names := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		names = append(names, f.Name)
	}
	if len(resp.Files) != 2 {
		t.Fatalf("root 下的散图也该显示（否则用户找不到这些文件），实际 %d 个: %v", len(resp.Files), names)
	}
	if resp.Total != len(resp.Dirs)+len(resp.Files) {
		t.Errorf("total 应是条目总数，实际 %d（dirs=%d files=%d）",
			resp.Total, len(resp.Dirs), len(resp.Files))
	}

	// 排序：目录段按名字升序 —— 2023 在 2024 之前（合并层要有统一顺序）
	if len(resp.Dirs) == 2 && resp.Dirs[0].Name != "2023" {
		t.Errorf("目录段应按名称排序，实际首项 %q", resp.Dirs[0].Name)
	}
}

func Test2418EmptyRootFallsBackToItself(t *testing.T) {
	base := t.TempDir()
	empty := filepath.Join(base, "空的图库")
	mkdirs(t, empty)

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{empty}})
	resp := aggregateRoot("name")

	if len(resp.Dirs) != 1 {
		t.Fatalf("★ 空 root 应回退成「它自己」这一项，实际 %d 项: %v", len(resp.Dirs), resp.Dirs)
	}
	if !samePath(resp.Dirs[0].Path, empty) {
		t.Errorf("回退项必须是 root 本身，实际 %q", resp.Dirs[0].Path)
	}
}

func Test2418EmptyAfterSkippingSystemDirsFallsBack(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "只剩回收站")
	mkdirs(t, filepath.Join(root, "#recycle"))
	touch(t, filepath.Join(root, "#recycle", "x.jpg"))

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{root}})
	resp := aggregateRoot("name")

	// 展开后因为跳过了 #recycle 而变空 ⇒ 必须回退成 root 自己，
	// 否则这个文件夹在首页彻底消失，用户以为添加失败。
	if len(resp.Dirs) != 1 || !samePath(resp.Dirs[0].Path, root) {
		t.Fatalf("★ 跳完系统目录后为空也要回退显示 root 自己，实际 %v", resp.Dirs)
	}
}

func Test2418MultipleRootsMergeSorted(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "A相册")
	b := filepath.Join(base, "B相册")
	mkdirs(t,
		filepath.Join(a, "2021"), filepath.Join(a, "2023"),
		filepath.Join(b, "2022"), filepath.Join(b, "2024"),
	)

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{a, b}})
	resp := aggregateRoot("name")

	var got []string
	for _, d := range resp.Dirs {
		got = append(got, d.Name)
	}
	want := []string{"2021", "2022", "2023", "2024"}
	if len(got) != len(want) {
		t.Fatalf("两个 root 的内容应合并成一层，期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("★ 合并后必须按同一套规则重排（否则看起来像按文件夹分组），期望 %v，实际 %v", want, got)
		}
	}
}

func Test2418MissingRootStillSkipped(t *testing.T) {
	base := t.TempDir()
	ok1 := filepath.Join(base, "在的")
	mkdirs(t, filepath.Join(ok1, "子目录"))

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{ok1, filepath.Join(base, "没挂载")}})
	resp := aggregateRoot("name")

	if len(resp.Dirs) != 1 || resp.Dirs[0].Name != "子目录" {
		t.Fatalf("缺失的 root 应跳过、在的 root 照常展开，实际 %v", resp.Dirs)
	}
}

// Test2418HideOnlyOnRoot 系统目录只从**首页**隐藏，点进去仍然看得到。
//
// 这一条防的是"顺手把 listDir 也改了"——那会让用户在目录里永远找不到回收站。
func Test2418HideOnlyOnRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "库")
	recycle := filepath.Join(root, "#recycle")
	mkdirs(t, recycle)
	touch(t, filepath.Join(recycle, "x.jpg"))

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{root}})
	resp := aggregateRoot("name")
	for _, d := range resp.Dirs {
		if d.Name == "#recycle" {
			t.Fatal("首页不该出现 #recycle")
		}
	}

	// 直接列 root：listDir 不受首页规则影响，仍然要能看到 #recycle
	sub, err := listDir(root, "name")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range sub.Dirs {
		if d.Name == "#recycle" {
			found = true
		}
	}
	if !found {
		t.Error("★ 进入目录后仍应能看到 #recycle（只在首页隐藏，不要在列表层改）")
	}
	_ = recycle
}

// ---------- 前端：面包屑与"后端到首页"的配套 ----------

// Test2418CrumbIsRelativeToRoot 面包屑只显示相对于「已添加文件夹」的段。
//
// 展开模式下进到 root 的子目录，若按绝对路径分段会变成
// 「我的文件夹 / vol1 / 1000 / 照片库 / 2024」—— 前几段是纯噪音。
func Test2418CrumbIsRelativeToRoot(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
function grab(name) {
  var i = appjs.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var depth = 0, j = appjs.indexOf('{', i);
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(i, k + 1); }
  }
  console.error('FAIL ' + name + ' 括号不配平'); process.exit(1);
}
function eq(a, b, m) {
  var sa = JSON.stringify(a), sb = JSON.stringify(b);
  if (sa !== sb) { console.error('FAIL ' + m + ' | expect ' + sb + ' got ' + sa); process.exit(1); }
}

var cfg = { onlyRoots: true, roots: ['/vol1/1000/照片库', '/vol2/1000/视频库'] };
var code =
  grab('normPath') + ';' + grab('relSegsUnderRoot') + ';' + grab('isCustomRoot') + ';' +
  'return { rel: relSegsUnderRoot, isRoot: isCustomRoot };';
var A = new Function('cfg', code)(cfg);

// 在 root 之下：只给相对段
eq(A.rel('/vol1/1000/照片库/2024'), { base: '/vol1/1000/照片库', segs: ['2024'] }, '一层子目录');
eq(A.rel('/vol1/1000/照片库/2024/春节'), { base: '/vol1/1000/照片库', segs: ['2024', '春节'] }, '两层子目录');
// 正好是 root：seg 空（面包屑只留首段）
eq(A.rel('/vol1/1000/照片库'), { base: '/vol1/1000/照片库', segs: [] }, '停在 root 本身');
// 不在任何 root 下：null（走完整分段）
eq(A.rel('/vol1/1000/别的目录'), null, '不在已添加文件夹之下');
// 前缀相同但不是子路径：必须是 null，不能被 '/照片库2' 误判为子路径
eq(A.rel('/vol1/1000/照片库2/2024'), null, '★ 前缀相似但不是子路径（不能靠字符串 startsWith 直接判）');
// 尾部斜杠：等价
eq(A.rel('/vol1/1000/照片库/2024/'), { base: '/vol1/1000/照片库', segs: ['2024'] }, '尾部斜杠应等价');
// Windows 反斜杠：root 与当前路径都用反斜杠也要能比。
// 用码点构造 String.fromCharCode(92)，避免"Go raw string → JS 字面量"两层转义
// 叠在一起时数不清到底几个反斜杠（我第一次就被吃成了正斜杠）。
var BS = String.fromCharCode(92);
var cfg2 = { onlyRoots: true, roots: ['D:' + BS + 'lib' + BS + '照片'] };
var B = new Function('cfg', code)(cfg2);
eq(B.rel('D:' + BS + 'lib' + BS + '照片' + BS + '2024'),
   { base: 'D:/lib/照片', segs: ['2024'] }, '反斜杠路径');

eq(A.isRoot('/vol1/1000/照片库'), true, 'isCustomRoot 命中');
eq(A.isRoot('/vol1/1000/照片库/2024'), false, 'isCustomRoot 不命中子目录');
// 未开启该选项：一律 null / false（不能影响普通模式的面包屑）
var cfg3 = { onlyRoots: false, roots: ['/vol1/1000/照片库'] };
var C = new Function('cfg', code)(cfg3);
eq(C.rel('/vol1/1000/照片库/2024'), null, '未开启时返回 null');
eq(C.isRoot('/vol1/1000/照片库'), false, '未开启时 isCustomRoot 恒 false');

console.log('OK');
`
	cmd := exec.Command(node, "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("面包屑探针失败：%v\n%s", err, out)
	}
}

// Test2418HomeRendersFiles 首页必须接住 files，且与 dirs 用同一套排序。
//
// 分工要说清（我第一次写错过）：显示由 state.items 负责（dirs + files）；
// state.files 是"这个目录的文件集合"，供后台预生成与设置页的「立即生成」使用。
// 所以这条断言守的是"首页的散图也会被预生成"，不是"不填就不显示"。
// 显示那一半由 tools/probe_addroot.js 的格子数断言覆盖。
func Test2418HomeRendersFiles(t *testing.T) {
	raw, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)

	i := strings.Index(js, "function buildRoot()")
	if i < 0 {
		t.Fatal("找不到 buildRoot")
	}
	end := strings.Index(js[i:], "\n  }")
	if end < 0 {
		t.Fatal("buildRoot 没有正常结束")
	}
	// 只取**成功分支**：catch 里清空 state.items/files 是正确的（失败时不该留旧格子），
	// 那是另一件事，不能把那条也算进来。
	rest := js[i:]
	cut := strings.Index(rest, "}).catch(")
	if cut < 0 {
		t.Fatal("buildRoot 里找不到 .catch 分支（结构变了？）")
	}
	body := rest[:cut]
	_ = end

	// 2.4.40：为了"接口回来一致就不重建"，items 先算进 nextItems 再赋值（语义没变）。
	for _, want := range []string{
		"state.files = rootFiles",
		"nextItems = rootDirs.concat(rootFiles)",
		"state.items = nextItems",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("★ buildRoot 缺少 %q —— 首页的散图不会被计入文件集合，"+
				"后台预生成会跳过它们（含设置页的「立即生成本目录缩略图」）", want)
		}
	}
	if strings.Contains(body, "state.files = [];") {
		t.Error("★ buildRoot 里仍有 `state.files = []` —— 首页返回的散图不会进入文件集合，" +
			"预生成与「立即生成」都会漏掉它们")
	}
}
