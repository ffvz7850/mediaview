//go:build windows

package main

import (
	"os"
	"syscall"
)

// fileBirthTime 取文件创建时间的毫秒时间戳。
//
// Windows 的 os.Stat 结果里带 CreationTime（NTFS 原生记录创建时间），
// 这是真·创建时间，不是 ctime 那种「元数据变更时间」。
//
// 这个实现存在的意义有两层：
//  1. 交叉编译到 Linux 之外时（开发机、跑 go test）代码能编过；
//  2. 本机 go test 时「按创建时间排序」这条链路有真实数据可断言，
//     而不是只在 Linux 上跑得通、开发机上一编译就红。
func fileBirthTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.CreationTime.Nanoseconds() / 1e6
	}
	return 0
}
