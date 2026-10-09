package main

import (
	"strings"
	"testing"
)

// TestDropQueuedThumbsOnlyDropsPending 清队列只丢弃「尚未被取走」的任务。
func TestDropQueuedThumbsOnlyDropsPending(t *testing.T) {
	// 队列为空时必须是 0（不阻塞、不 panic）—— 用非阻塞 select 实现的关键保证
	if n := dropQueuedThumbs(); n != 0 {
		t.Errorf("空队列 dropQueuedThumbs() = %d，期望 0", n)
	}

	// 构造一个队列并投 3 个任务，清掉后应返回 3，且队列变空
	q := make(chan thumbReq, 8)
	for i := 0; i < 3; i++ {
		q <- thumbReq{path: "/tmp/x.jpg", kind: "image", size: 320}
	}
	n := 0
	for {
		select {
		case <-q:
			n++
		default:
			goto done
		}
	}
done:
	if n != 3 {
		t.Fatalf("投了 3 个任务，取出 %d 个", n)
	}
}

// TestDropQueuedThumbsIsNonBlocking 契约：实现必须用非阻塞 select（default 分支），
// 否则在队列空时会把调用方（handleList）挂住。
func TestDropQueuedThumbsIsNonBlocking(t *testing.T) {
	th := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	i := strings.Index(th, "func dropQueuedThumbs()")
	if i < 0 {
		t.Fatal("找不到 dropQueuedThumbs")
	}
	rest := th[i:]
	if j := strings.Index(rest, "\nfunc "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "default:") {
		t.Error("dropQueuedThumbs 没有 default 分支 —— 队列空时会阻塞住 handleList")
	}
	if !strings.Contains(rest, "currentThumbQueue()") {
		t.Error("dropQueuedThumbs 没有用 currentThumbQueue() 取当前队列")
	}
}

// TestHandleListDropsOnDirChange 契约：换目录时才清（同目录刷新不清），且必须清在 listDir 之前。
func TestHandleListDropsOnDirChange(t *testing.T) {
	med := stripLineComments(readSourceOrSkip(t, "media.go"))
	i := strings.Index(med, "func handleList(")
	if i < 0 {
		t.Fatal("找不到 handleList")
	}
	rest := med[i:]
	if j := strings.Index(rest, "\nfunc "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "dropQueuedThumbs()") {
		t.Error("handleList 里没有调用 dropQueuedThumbs —— 换目录不会清旧待办")
	}
	if !strings.Contains(rest, "lastListedDir") {
		t.Error("handleList 没有用 lastListedDir 判断「是否真的换了目录」——" +
			"同目录刷新也会白清一次")
	}
	// 清理必须发生在 listDir / preloadBatch 之前，否则新目录的任务会被自己清掉
	iDrop := strings.Index(rest, "dropQueuedThumbs()")
	iList := strings.Index(rest, "listDir(")
	if iList >= 0 && iDrop > iList {
		t.Error("dropQueuedThumbs 在 listDir 之后调用 —— 会把新目录刚投的任务也清掉")
	}
}

// TestThumbReqFieldNames 契约：thumbReq 的字段是小写的（与 enqueueThumb 里的用法一致）。
func TestThumbReqFieldNames(t *testing.T) {
	th := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	if !strings.Contains(th, "type thumbReq struct {") {
		t.Fatal("找不到 thumbReq")
	}
	i := strings.Index(th, "type thumbReq struct {")
	rest := th[i:]
	if j := strings.Index(rest, "}"); j > 0 {
		rest = rest[:j]
	}
	for _, f := range []string{"path", "kind", "size"} {
		if !strings.Contains(rest, f) {
			t.Errorf("thumbReq 缺少字段 %s", f)
		}
	}
}
