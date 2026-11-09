//go:build linux

package main

import "golang.org/x/sys/unix"

// coverMetaXattr 是封面元数据（coverPick）存放的扩展属性名。
//
// 为什么不再用 .json sidecar：一个文件夹封面会变成「一张图 + 一个 json」两个文件，
// dircov/ 下的文件数直接翻倍，用户看到的就是"每个封面旁边多一个 json"。
// xattr 让元数据**跟着封面图本身走**，目录里只剩一张 jpg。
//
// 这不是新发明：缩略图在 1.8.148 已经把 .key sidecar 换成了 xattr
// （见 thumbxattr_linux.go），这里用的是同一套思路、只是换了个属性名，
// 避免与缩略图那条 key 互相覆盖。
const coverMetaXattr = "user.mediaview.dircov"

// coverMetaXattrBuf 读取缓冲上限。
//
// ⚠ 不能用缩略图那条的 512 字节：封面 meta 实测常见 486 字节、
// 满配（4 张图 + 32 个目录检查、全中文长路径）约 3838 字节。
// 缓冲小了会**静默截断** —— json.Unmarshal 失败 → 判定"没有元数据" →
// 每次访问都重新挑图重新合成，而且不报任何错（这条踩过，别改小）。
const coverMetaXattrBuf = 8192

func setCoverMetaXattr(jpgPath string, raw []byte) error {
	if len(raw) > coverMetaXattrLimit {
		return errCoverMetaTooLarge
	}
	return unix.Setxattr(jpgPath, coverMetaXattr, raw, 0)
}

func getCoverMetaXattr(jpgPath string) ([]byte, bool) {
	buf := make([]byte, coverMetaXattrBuf)
	n, err := unix.Getxattr(jpgPath, coverMetaXattr, buf)
	if err != nil || n <= 0 {
		return nil, false
	}
	return buf[:n], true
}
