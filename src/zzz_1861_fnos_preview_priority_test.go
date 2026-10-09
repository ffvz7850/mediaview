package main

// 1.8.61：飞牛文件管理器「打开大图浏览窗口不必先跑完一页缩略图」。
//
// 现场：文件管理器打开一个未缓存目录时，会往 /thumb/getIcon 连发一整页 size=list 请求。
// 单张 320px 缩略图要从 6000+ 像素的原图整帧解码（本机实测 0.6s，NAS 更慢），
// 一页按 50 张、8 并发算就是 4 秒起步。此时用户点开大图预览（size=big），旧实现里
// 它和网格请求共用同一个「带缓冲 channel 做的信号量」，取槽严格 FIFO ——
// 大图排在整页 list 后面，所以用户必须先等一页缩略图。
//
// 修法两条，缺一不可：
//   A. size=big 触发 noteForegroundPreview()：后台预生成停摆 + kill 在跑的预生成 ffmpeg；
//      （此前 1.8.56 把这个钩子删掉了，理由是「文件管理器从不请求 big」——已证伪：
//      飞牛 ImagePlayer-*.js 里就是 `{size:B.Big,path:e[t]}`）
//   B. 这一张自己走**预留槽**、且不让路：不排 thumbGenSem 的队，也不会「触发暂停后
//      再等自己触发的让路窗口」。
//
// 本文件守 A 的接线与 B 的行为。为什么 B 在 ensureThumbSys 层测而不是 handler 层：
// handleSystemThumbGetIcon 里 path 要先过 parseSystemThumbPath→resolveMediaPath，
// Windows 上 /volN/... 必然 400 提前返回，在 handler 层断言会得到假阴性。
//
// 断言都做了「超时即失败」的护栏，而不是让用例挂死：挂死同样能证明有 bug，
// 但 10 分钟超时给不出定位信息（M1 变异体第一次跑就是这个形态）。

import (
	"os"
	"strings"
	"testing"
	"time"
)

// holdAllFreeThumbSlots 把全局缩略图信号量当前所有空余槽全部占住，
// 返回放槽函数。目的是构造「网格已经把并发槽排满」的现场：这时普通前台请求必须阻塞，
// 而大图预览请求必须照常通过（它走预留槽）。
//
// 用非阻塞取槽而不是「往 channel 里塞 cap 个 token」：后台 worker 可能正握着几个槽，
// 硬塞会把测试自己堵死（本文件第一版就是这么挂的）。非阻塞取法既不会死锁，
// 也照样能把信号量占满。
func holdAllFreeThumbSlots() func() {
	sem := getThumbSem()
	took := 0
	for {
		select {
		case sem <- struct{}{}:
			took++
		default:
			// 释放必须幂等：这条用例会显式放一次、再 defer 放一次，
			// 放两次会多排空若干 token，把后面所有缩略图请求永久堵死。
			released := false
			return func() {
				if released {
					return
				}
				released = true
				for i := 0; i < took; i++ {
					<-sem
				}
			}
		}
	}
}

type thumbCallResult struct {
	d   time.Duration
	err error
}

// callTimeout 在 goroutine 里调 ensureThumbSys，超过 limit 就判定为「被堵住」，
// 并回调 unblock（放槽）让被堵住的调用能收尾，避免用例挂死。
func callTimeout(t *testing.T, limit time.Duration, unblock func(),
	size int, immediate bool, src string, what string) (thumbCallResult, bool) {
	t.Helper()
	ch := make(chan thumbCallResult, 1)
	go func() {
		t0 := time.Now()
		_, err := ensureThumbSys(src, "image", size, immediate)
		ch <- thumbCallResult{time.Since(t0), err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%s 生成失败：%v", what, r.err)
		}
		return r, true
	case <-time.After(limit):
		unblock()
		t.Errorf("%s 超过 %v 仍没返回 —— 它被排进网格请求的 FIFO 队列了，"+
			"用户还得先等一页缩略图", what, limit)
		select {
		case <-ch:
		case <-time.After(15 * time.Second):
			t.Error("放槽后被堵住的请求仍未收尾")
		}
		return thumbCallResult{}, false
	}
}

// 大图预览请求在「网格已经把并发槽占满」时仍必须立刻干完。
// 这就是用户看到的差别：等一页缩略图（4s+） vs 只等自己这一张。
func Test1861BigPreviewBypassesSaturatedGridQueue(t *testing.T) {
	withThumbRootTemp(t)
	// 让后台预生成在这条用例里彻底不参与（否则它可能悄悄让出槽，污染前提）
	oldQuiet := thumbQuietPeriod
	thumbQuietPeriod = 30 * time.Second
	t.Cleanup(func() { thumbQuietPeriod = oldQuiet })
	resumeViaHTTP(t)
	t.Cleanup(func() { resumeViaHTTP(t) })

	src := writeRelativePNG(t, "zz_1861_src.png")
	// 先确认这台机器能生成缩略图（不能就跳过，避免把环境问题报成回归）
	if _, err := ensureThumb(src, "image", 64); err != nil {
		t.Skipf("本机无法生成缩略图，跳过：%v", err)
	}

	release := holdAllFreeThumbSlots()
	defer release()

	// ① immediate=true（大图预览）：必须绕开被占满的主信号量
	if r, ok := callTimeout(t, 3*time.Second, release, 512, true,
		src, "大图预览请求（immediate=true）"); ok {
		if r.d > 500*time.Millisecond {
			t.Errorf("大图预览用了 %v（主信号量被网格占满时它被堵住了）", r.d)
		} else {
			t.Logf("主信号量被占满时，大图预览仍只用 %v 出图", r.d.Round(time.Millisecond))
		}
	}

	// ② immediate=false（网格/卡片的另一档）：必须**排队**，证明主信号量确实被占满，
	//    否则①可能只是「信号量根本没满」造成的假阳性。
	blocked := make(chan thumbCallResult, 1)
	go func() {
		t0 := time.Now()
		_, err := ensureThumbSys(src, "image", 640, false)
		blocked <- thumbCallResult{time.Since(t0), err}
	}()
	select {
	case r := <-blocked:
		t.Errorf("主信号量已占满，普通请求却只用了 %v（err=%v）就返回 —— "+
			"前提不成立，① 的结论不可信", r.d, r.err)
	case <-time.After(400 * time.Millisecond):
		// 符合预期：普通请求在排队
	}
	release() // 放槽，让上面那个 goroutine 干完
	select {
	case r := <-blocked:
		if r.err != nil {
			t.Errorf("放槽后普通请求仍失败：%v", r.err)
		} else {
			t.Logf("放槽后普通请求完成，阻塞了约 %v（印证它确实在排队）", r.d.Round(time.Millisecond))
		}
	case <-time.After(15 * time.Second):
		t.Error("放槽后普通请求仍未返回，信号量没被正确释放")
	}

	// ③ 预留槽用完必须归还，否则第二次打开大图会永久阻塞
	if n := len(thumbImmediateSem); n != 0 {
		t.Errorf("预留槽没有归还：len=%d（下次打开大图会卡死）", n)
	}
}

// 预留槽必须足够小（它是「额外」的并发，用来换确定性，不能变成扩大总并发的手段），
// 且不能和主信号量是同一个对象 —— 同一个对象就又回到 FIFO 排队了。
func Test1861ImmediateSlotIsSmallAndSeparate(t *testing.T) {
	if cap(thumbImmediateSem) != 1 {
		t.Errorf("预留槽容量为 %d，期望 1", cap(thumbImmediateSem))
	}
	if len(thumbImmediateSem) != 0 {
		t.Errorf("预留槽初始不是空的：len=%d", len(thumbImmediateSem))
	}
	// 取一个预留槽，主信号量不能因此变化（是各自独立的两个对象）
	before := len(getThumbSem())
	release := acquireImmediateSlot()
	if after := len(getThumbSem()); after != before {
		t.Errorf("取预留槽动了主信号量：%d → %d", before, after)
	}
	if len(thumbImmediateSem) != 1 {
		t.Errorf("取预留槽之后 len=%d，期望 1", len(thumbImmediateSem))
	}
	release()
	if len(thumbImmediateSem) != 0 {
		t.Errorf("release 之后预留槽没归零：len=%d", len(thumbImmediateSem))
	}
}

// 接线守卫：big 必须①触发暂停 ②以 immediate 进入生成。
// 这两条任一被去掉，用户就回到「先跑完一页缩略图才出大图」。
func Test1861SystemThumbBigIsWiredToPreviewPriority(t *testing.T) {
	b, err := os.ReadFile("systemthumb.go")
	if err != nil {
		t.Skipf("读不到 systemthumb.go（测试 CWD 不是源码目录）：%v", err)
	}
	s := string(b)

	if !strings.Contains(s, "noteForegroundPreview()") {
		t.Error("systemthumb.go 里没有 noteForegroundPreview() —— " +
			"打开大图预览时后台预生成不再停摆")
	}
	if !strings.Contains(s, `if size == "big" {`) {
		t.Error(`systemthumb.go 不再按 size == "big" 触发大图预览语义 —— ` +
			`网格 list 与大图 big 又重新混为一谈`)
	}
	if !strings.Contains(s, `ensureSystemThumb(origPath, px, size == "big")`) {
		t.Error("systemthumb.go 没有把「这是大图预览」传给生成层（immediate 标志）—— " +
			"大图会重新排进整页 list 请求的 FIFO 队列")
	}
	// 反面：medium（卡片视图）**不得**触发大图预览暂停 —— 它一次来很多张，触发就是自己挡自己。
	//
	// 1.8.126 起 medium 会被归一化成 list（复用同一份 320 产物，见 handleSystemThumbGetIcon），
	// 所以源码里出现 `size == "medium"` 是**正常**的 —— 那是归一化，不是触发预览。
	// 因此判据不能只看出没出现这个字符串，要看它那个分支里**有没有**触发暂停。
	if i := strings.Index(s, `if size == "medium"`); i >= 0 {
		end := i + 400
		if end > len(s) {
			end = len(s)
		}
		if strings.Contains(s[i:end], "noteForegroundPreview") {
			t.Error(`medium（卡片视图）被当成大图预览触发暂停 —— ` +
				`卡片视图一次来很多张，触发暂停会自己挡自己`)
		}
	}
	// 预留槽入口仍在（大图请求要能绕过整页 list 请求的 FIFO 队列）
	if !strings.Contains(s, "ensureThumbSys(") {
		t.Error("systemthumb.go 不再调用 ensureThumbSys —— " +
			"文件管理器的大图预览会重新排进整页 list 请求的 FIFO 队列")
	}
}
