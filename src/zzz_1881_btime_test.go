package main

// 1.8.81：创建时间排序失效的词表缺口。
//
// 前端内部字段名是 btime（sortParts 的映射目标），而 parseSort 的别名表里
// 只有 btim / ctime / birthtime…，**没有 btime** —— 于是 `btime:asc` 无法识别、
// 退回默认 name，而且 needBtime 恒为 false（连 statx 都不会去取）。
// 现象：文件管理器按创建时间升序是 IMG_0182→IMG_0189→IMG_0184（statx btime 升序），
// 而大图却按名称排。
import (
	"os"
	"strings"
	"testing"
)

func Test1881BtimeAliasIsAccepted(t *testing.T) {
	for _, in := range []string{"btime", "btime:asc", "btime:desc", "btim:asc", "birthtime:asc"} {
		sp := parseSort(in)
		if sp.field != sortFieldBtime {
			t.Errorf("parseSort(%q) 得到字段 %q，期望 %q（创建时间）", in, sp.field, sortFieldBtime)
		}
		if !strings.HasPrefix(in, "btime:asc") && !strings.HasPrefix(in, "btim:asc") && !strings.HasPrefix(in, "birthtime:asc") {
			continue
		}
		if sp.field == sortFieldBtime && sp.desc {
			t.Errorf("parseSort(%q) 应为升序", in)
		}
	}
	// 回归：其它字段不受影响
	if sp := parseSort("name:asc"); sp.field != sortFieldName || sp.desc {
		t.Errorf("name:asc 解析异常：%+v", sp)
	}
	if sp := parseSort("mtime:desc"); sp.field != sortFieldMtime || !sp.desc {
		t.Errorf("mtime:desc 解析异常：%+v", sp)
	}
}

// 契约：needBtime 的判据必须能让 btime 排序真正去取创建时间。
func Test1881NeedBtimeWiring(t *testing.T) {
	src, err := os.ReadFile("scanner.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, "needBtime := sp.field == sortFieldBtime") {
		t.Error("scanner.go 没有按 sortFieldBtime 决定是否取创建时间")
	}
	if !strings.Contains(s, "fileBirthTime(full)") {
		t.Error("没有调用 fileBirthTime")
	}
	// FileItem.Btime 必须能出现在 JSON 里（前端 sortLikeFnOs 要用它比较）
	m, err := os.ReadFile("scanner.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m), "json:\"btime") {
		t.Error("FileItem.Btime 的 JSON tag 里没有 btime —— 前端拿不到该字段就无法排序")
	}
}
