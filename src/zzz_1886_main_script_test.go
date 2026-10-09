package main

import (
	"os"
	"strings"
	"testing"
)

// Test1886MainScriptPidReuseGuard 1.8.182：启动脚本必须防"PID 复用"。
//
// 实际踩到的故障（真机证据）：
//
//	mediaview.pid 里是 4464，而 /proc/4464/cmdline 是 "./duplicati-server"
//	—— mediaview 被 SIGTERM 杀掉后 PID 文件残留，那个号被别的进程复用了。
//	旧版 check_process 只看 kill -0 → 认为"在运行"：
//	  · status 返回 0 → 应用中心据此不再启动它；
//	  · start 直接 exit 0 → 根本没启动；
//	  · socket 不存在 → 网关转发失败 → 浏览器 502 Bad Gateway。
//
// 所以脚本里必须同时具备：
//  1. 用 /proc/<pid>/exe 比对二进制（确认那是本应用）；
//  2. exe 不可读时用 cmdline 兜底；
//  3. status 还要求 socket 存在（否则"半死"会被报成健康）；
//  4. stop 时先确认身份再 kill（避免误杀复用了该 PID 的无关进程）。
func Test1886MainScriptPidReuseGuard(t *testing.T) {
	b, err := os.ReadFile("cmd/main")
	if err != nil {
		// 源码包布局可能与打包目录不同，两种都试
		b, err = os.ReadFile("../fpk-x86/cmd/main")
		if err != nil {
			t.Skipf("找不到 cmd/main：%v", err)
		}
	}
	s := string(b)
	must := []struct{ frag, why string }{
		{"/proc/${pid}/exe", "必须用 /proc/<pid>/exe 校验进程身份（防 PID 复用）"},
		{"readlink -f", "必须解析真实二进制路径再比对"},
		{"/proc/${pid}/cmdline", "必须有 cmdline 兜底判据"},
		{`kill -0 "$pid" 2>/dev/null || return 1`, "check_process 必须保留存在性检查（不能只看身份）"},
		{`check_process "$pid" && [ -S "$SOCK" ]`, "status 必须同时要求「进程是我们的」且「socket 在」—— 否则半死状态被报成健康，应用中心不会重启它 → 502"},
		{"PID ${pid} 存在但不是 mediaview", "识别出 PID 复用时必须留下日志，便于事后诊断"},
		{"check_process", "身份校验要收敛到 check_process"},
	}
	for _, m := range must {
		if !strings.Contains(s, m.frag) {
			t.Errorf("cmd/main 缺少 %q —— %s", m.frag, m.why)
		}
	}
	// 反向断言：不允许出现"只看 kill -0 就 return 0"的写法
	if strings.Contains(s, "if [ -n \"$pid\" ] && kill -0 \"$pid\" 2>/dev/null; then\n        return 0") {
		t.Error("cmd/main 的 check_process 又退回「只看 kill -0」—— PID 复用会被误判为在运行")
	}
	// stop 必须先验身份再 kill
	i := strings.Index(s, "stop)")
	if i > 0 {
		tail := s[i:]
		if j := strings.Index(tail, ";;"); j > 0 {
			tail = tail[:j]
		}
		if !strings.Contains(tail, "check_process") {
			t.Error("stop 分支必须先确认进程身份再 kill —— 否则会误杀复用了该 PID 的无关进程")
		}
	}
}

// Test1886MainScriptBothArch 两个架构的启动脚本必须一致（否则 arm 装机仍有该 bug）。
func Test1886MainScriptBothArch(t *testing.T) {
	a, err1 := os.ReadFile("../fpk-x86/cmd/main")
	b, err2 := os.ReadFile("../fpk-arm/cmd/main")
	if err1 != nil || err2 != nil {
		t.Skip("源码包布局不含 fpk-*，跳过（打包流程单独校验一致性）")
	}
	if string(a) != string(b) {
		t.Error("fpk-x86 与 fpk-arm 的 cmd/main 不一致 —— arm 装机仍会有 PID 复用问题")
	}
}
