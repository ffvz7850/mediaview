//go:build !linux

package main

// 非 Linux 平台没有 user.* 扩展属性：写入直接报"不支持"、读取永远 miss。
// 调用方（saveCoverPick / loadCoverPick）会因此回退到 .json sidecar ——
// 功能不降级，只是目录里仍然会多一个文件。
//
// 这个分支同时是 Windows 开发机上的默认路径，所以本机的端到端测试
// 只能验证"回退"那一半；xattr 正路靠测试注入内存实现来覆盖
// （见 zzz_2423_cover_meta_test.go 里的 stubCoverMeta）。
func setCoverMetaXattr(jpgPath string, raw []byte) error { return errCoverXattrUnsupported }

func getCoverMetaXattr(jpgPath string) ([]byte, bool) { return nil, false }
