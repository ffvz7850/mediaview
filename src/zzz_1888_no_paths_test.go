package main

import (
	"strings"
	"testing"
)

// Test1888NoPathsBranch 1.8.188：`paths`（宿主有序列表）分支**不得复活**。
//
// 删除依据（全量 nginx access.log 统计）：
//
//	open 请求总数 2013，其中带 paths= 的 **0 条**；
//	飞牛前端里 `paths=` 只出现在"文件夹选择 / 分享 / 分享链接 / PDF"，与"打开大图"无关。
//
// 它是早期版本遗留、实测从未出现；而且若真出现，它给的顺序与"用户当前在文件管理器里
// 设的排序"可能冲突，反而把顺序带偏 —— 所以整条分支连同 parsePathsParam 一并删除。
//
// 现在的排序来源（收敛为两条）：
//
//	大图 → URL 的 sort（若有）→ IndexedDB 偏好 → name:asc
//	网格 → state.sort
func Test1888NoPathsBranch(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")
	s := stripJSComments(js)
	for _, bad := range []string{"parsePathsParam", "openSingle(single, null, paths)"} {
		if strings.Contains(s, bad) {
			t.Errorf("app.js 里又出现 %s —— paths 分支实测 0/2013 从未出现，且会与用户当前排序冲突", bad)
		}
	}
	if strings.Contains(s, "function openSingle(path, sort, paths)") {
		t.Error("openSingle 又接收 paths 参数了 —— 该来源已废除")
	}
	// 反向确认：真正在用的两条来源必须都在
	if !strings.Contains(s, "window.FnOsSort.getDirSort(") {
		t.Error("大图必须读 IndexedDB 偏好（实测 93% 的打开大图不带 sort，只能靠它）")
	}
	if !strings.Contains(s, "if (!force) return Promise.resolve(state.sort);") {
		t.Error("网格必须用 state.sort")
	}
}
