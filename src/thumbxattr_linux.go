//go:build linux

package main

import "golang.org/x/sys/unix"

// thumbKeyXattr 是缩略图内容标识存放的扩展属性名。
//
// 为什么用 xattr 而不是 sidecar 文件：见 thumb.go 里 thumbCacheKeyMatches 的注释 ——
// 缓存目录原本是「一张图 + 一个 .key + 一个 .meta.json」，
// 文件数是实际缩略图的三倍。xattr 让标识**跟着产物文件本身走**，不再多占一个文件。
const thumbKeyXattr = "user.mediaview.key"

func setThumbKeyXattr(out, key string) error {
	return unix.Setxattr(out, thumbKeyXattr, []byte(key), 0)
}

func getThumbKeyXattr(out string) (string, bool) {
	buf := make([]byte, 512)
	n, err := unix.Getxattr(out, thumbKeyXattr, buf)
	if err != nil || n <= 0 {
		return "", false
	}
	return string(buf[:n]), true
}
