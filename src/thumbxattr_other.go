//go:build !linux

package main

// 非 Linux 平台没有 user.* xattr：写入与读取都返回"不支持"，
// 由调用方回退（见 thumb.go thumbCacheKeyMatches 的三级回退）。
func setThumbKeyXattr(out, key string) error { return nil }

func getThumbKeyXattr(out string) (string, bool) { return "", false }
