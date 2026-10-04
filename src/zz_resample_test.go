package main

import (
	"strings"
	"testing"
)

// 缩图滤镜链的语义是画质契约，改动必须被拦住。
//
// 历史上这里出过两类问题：
//   - 目标框直接写 scale=N:N —— 目标框只负责"适应"、并不阻止放大，1200px 的小图
//     会被拉到 2400px 再重编码，比原图更糊（1.8.16 用 min(maxdim,iw/ih) 修掉）；
//   - 大幅降采样（6000→2400）沿用默认的 bicubic 会产生混叠，细节出现锯齿/摩尔纹。
//
// 缓存版本标记（scaledCacheTag）另有既有断言覆盖：产物文件名必须包含它，
// 否则旧算法生成的图会被继续命中。
func TestImageScaleFilterShrinksOnly(t *testing.T) {
	got := imageScaleFilter(2400)

	for _, want := range []string{
		"min(2400,iw)", "min(2400,ih)", // 只缩不放
		"force_original_aspect_ratio=decrease", // 等比
		"flags=lanczos",                        // 大幅降采样不混叠
	} {
		if !strings.Contains(got, want) {
			t.Errorf("滤镜链缺少 %q\n实际: %s", want, got)
		}
	}

	// maxdim 必须真的参与拼接，不能被写死
	if other := imageScaleFilter(1024); !strings.Contains(other, "min(1024,iw)") {
		t.Errorf("maxdim 未生效于滤镜链: %s", other)
	}
}
