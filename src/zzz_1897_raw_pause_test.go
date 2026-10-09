package main

// 1.8.97：大图请求必须暂停**前台**缩略图生成。
//
// 实测环境：NAS 4 核，thumbConcurrency=4。
// /api/raw 缓存命中只要 11ms，冷转码 567ms —— 但用户点开一张没缓存的大图时，
// 网格里 4 个 size=list 缩略图转码正把 4 个核占满，大图转码拿不到 CPU，
// 表现就是「要等跑完两页缩略图，大图才出来」。
//
// handleRaw 原来只调 touchThumbActivity()（后台预生成让路），漏了
// noteForegroundPreview()（强制暂停 + 让缩略图请求返回占位图）。
// 飞牛文件管理器的 size=big 预览走的就是后者（systemthumb.go），注释里写得很清楚：
// 「打开大图预览时**不必先跑完一整页缩略图**」。
import (
	"os"
	"strings"
	"testing"
)

func Test1897RawPausesForegroundThumbs(t *testing.T) {
	b, err := os.ReadFile("media.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	i := strings.Index(s, "func handleRaw(")
	if i < 0 {
		t.Fatal("找不到 handleRaw")
	}
	seg := s[i:]
	if j := strings.Index(seg, "\nfunc "); j > 0 {
		seg = seg[:j]
	}

	if !strings.Contains(seg, "noteForegroundPreview()") {
		t.Error("handleRaw 没有调用 noteForegroundPreview() —— 网格的前台缩略图会继续抢 CPU，" +
			"大图要等整页缩略图跑完（4 核机上尤其明显）")
	}
	if !strings.Contains(seg, "touchThumbActivity()") {
		t.Error("handleRaw 丢了 touchThumbActivity()（后台预生成的让路）")
	}
	// 顺序：先打点、再暂停，与 systemthumb.go 保持一致
	if strings.Index(seg, "touchThumbActivity()") > strings.Index(seg, "noteForegroundPreview()") {
		t.Error("handleRaw 里两个让路调用的顺序不对（应先 touchThumbActivity 再 noteForegroundPreview）")
	}
}

// 浏览类接口（list/scan/meta）**不能**暂停前台缩略图 —— 那会把网格自己的
// 缩略图请求挡掉（打开目录时首批缩略图全变占位图）。
func Test1897BrowseDoesNotPause(t *testing.T) {
	b, err := os.ReadFile("media.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, fn := range []string{"func handleList(", "func handleScan(", "func handleMeta("} {
		i := strings.Index(s, fn)
		if i < 0 {
			t.Fatalf("找不到 %s", fn)
		}
		seg := s[i:]
		if j := strings.Index(seg, "\nfunc "); j > 0 {
			seg = seg[:j]
		}
		if strings.Contains(seg, "noteForegroundPreview()") {
			t.Errorf("%s 里不应调用 noteForegroundPreview() —— 浏览目录时暂停缩略图，"+
				"会让网格首批缩略图全变成占位图", fn)
		}
	}
}
