package main

import (
	"sort"
	"testing"
)

// Test1883BackendNaturalLess 1.8.180：排序的唯一实现是后端 naturalLess。
//
// 它必须满足"与飞牛文件管理器逐字一致"的两条：连续数字按**数值**比（IMG_2 < IMG_10），
// 其余按字节序；且主字段相等时**不兜底**（返回 false，配合 SliceStable 保持原顺序）。
func Test1883BackendNaturalLess(t *testing.T) {
	// 数字按值：A9 < A10，IMG_2 < IMG_10
	cases := []struct {
		a, b string
		want bool
	}{
		{"A9.jpg", "A10.jpg", true},
		{"IMG_2.jpg", "IMG_10.jpg", true},
		{"A10.jpg", "A9.jpg", false},
		{"(1).jpg", "(2).jpg", true},
		{"(2).jpg", "(10).jpg", true},
		{"a.jpg", "b.jpg", true},
		{"b.jpg", "a.jpg", false},
	}
	for _, c := range cases {
		if got := naturalLess(c.a, c.b); got != c.want {
			t.Errorf("naturalLess(%q, %q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// Test1883LessStableOnEqual 主字段相等时必须返回 false（不自己兜底），
// 这样配合 sort.SliceStable 才能保持 os.ReadDir 的原始顺序 —— 与飞牛一致。
func Test1883LessStableOnEqual(t *testing.T) {
	sp := parseSort("mtime:desc")
	a := sortKey{Name: "a.jpg", Mtime: 100}
	b := sortKey{Name: "b.jpg", Mtime: 100}
	if a.less(b, sp) || b.less(a, sp) {
		t.Error("mtime 相等时 less 应两边都返回 false（不兜底），否则与飞牛的先后会不一致")
	}
	// 方向必须有效
	sp2 := parseSort("mtime:asc")
	x := sortKey{Name: "x.jpg", Mtime: 100}
	y := sortKey{Name: "y.jpg", Mtime: 200}
	if !x.less(y, sp2) {
		t.Error("mtime:asc 下 100 应排在 200 之前")
	}
	if x.less(y, sp) {
		t.Error("mtime:desc 下 100 不应排在 200 之前")
	}
}

// Test1883SortAllFieldsAscDesc 五种字段的升/降序必须真的相反（防止方向丢失）。
func Test1883SortAllFieldsAscDesc(t *testing.T) {
	base := []sortKey{
		{Name: "a.jpg", Mtime: 300, Btime: 30, Size: 10, Ext: ".jpg"},
		{Name: "b.png", Mtime: 100, Btime: 10, Size: 30, Ext: ".png"},
		{Name: "c.gif", Mtime: 200, Btime: 20, Size: 20, Ext: ".gif"},
	}
	order := func(spec string) string {
		ks := append([]sortKey(nil), base...)
		sp := parseSort(spec)
		sort.SliceStable(ks, func(i, j int) bool { return ks[i].less(ks[j], sp) })
		s := ""
		for _, k := range ks {
			s += k.Name[:1]
		}
		return s
	}
	for _, f := range []string{"mtime", "btime", "size", "type", "name"} {
		up, dn := order(f+":asc"), order(f+":desc")
		if up == dn {
			t.Errorf("%s 的 asc 与 desc 结果相同（%s）—— 方向没生效", f, up)
		}
	}
}
