//go:build linux

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// fileBirthTime 取文件创建时间的毫秒时间戳（statx 的 btime）。
//
// 为什么不是 os.FileInfo.ModTime()：那是**修改时间**，与「创建时间」列不是一回事。
// 为什么不是 syscall.Stat_t.Ctim：那是 inode 元数据变更时间 —— chmod、改名、
// 建硬链接都会更新它，批量整理过目录后 ctime 会集体变成整理那天，
// 按它排序和文件管理器里按「创建时间」排的结果对不上。
//
// 三层兜底（越靠后越不精确，但保证有值可用）：
//
//  1. statx 的 btime            —— 内核 >= 4.11 且文件系统支持（btrfs/ext4/xfs 支持）
//  2. inode 变更时间 ctime       —— 老内核或文件系统不支持 btime 时
//  3. 0                          —— 连 stat 都失败（权限/刚被删除），排序时垫底
//
// 注意 AT_SYMLINK_NOFOLLOW：与 os.Lstat 语义一致。目录里可能有指向别处的符号
// 链接，跟随链接会把时间显示成目标文件的，排序跟着错。
//
// 精度：**截断到秒**。飞牛文件管理器的「创建时间」精确到秒，同一秒内创建的文件
// 在它那边是相等的，先后由目录物理顺序决定。这里若保留毫秒/纳秒，本应用会把同秒
// 文件再按亚秒排开，先后就与文件管理器不一致（实测 3.25 目录：1379/1372/1345 的
// btime 落在同一秒的 4 毫秒内，文件管理器按物理顺序给出 1379→1372→1345，
// 而按毫秒排会得到 1372→1345→1379）。
func fileBirthTime(path string) int64 {
	var stx unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx)
	if err == nil && stx.Mask&unix.STATX_BTIME != 0 {
		return time.Unix(stx.Btime.Sec, 0).UnixMilli()
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err == nil {
		return time.Unix(st.Ctim.Sec, 0).UnixMilli()
	}
	return 0
}
