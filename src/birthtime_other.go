//go:build !linux && !windows

package main

// fileBirthTime 在既没有 statx 也没有 NTFS 创建时间的平台上返回 0。
//
// 返回 0 而不是退回 ModTime：0 在排序里会稳定地垫底，用户能一眼看出
// 「创建时间这一列在这个平台上没有数据」；而悄悄拿修改时间冒充创建时间，
// 会得到一个「看起来正常但和文件管理器对不上」的顺序，更难排查。
// 本应用实际只发布 linux/amd64 与 linux/arm64，这个文件只为可移植性存在。
func fileBirthTime(path string) int64 { return 0 }
