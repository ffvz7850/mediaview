package main

// 基准测量：同一张真实照片生成飞牛文件管理器 list/medium/big 三档缩略图的耗时。
//
// 目的：量化「接管系统缩略图后，同一张原图被反复解码」的代价。
// 事实基础（见 cache.go / thumb.go）：
//   - 缓存路径 thumbPathFor(path,size) = <root>/<size>/<相对路径> → 三档各自一份；
//   - ensureThumbInternal 的 singleflight key 含 size → 三种尺寸不会互相复用结果；
//   - mediaview 前端网格用 thumbRequestSize()（默认 320），仅与 list 档可能重合。
//
// Windows 上 thumbPathFor 不能处理带盘符的绝对路径（会把 "Z:" 当目录名，mkdir 必败），
// 所以先把样张复制成 CWD 下的相对路径再测。无 Z 盘 / 无 ffmpeg 时自动跳过。

import (
	"bytes"
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestZZBench1850FnosThreeTierCost(t *testing.T) {
	const srcPhoto = `Z:\压缩旅游不带视频\2025.3压缩\IMG_5164.JPG`
	if _, err := os.Stat(srcPhoto); err != nil {
		t.Skip("Z 盘样张不可用，跳过")
	}
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg

	settingsMu.Lock()
	oldOn, oldQ := settings.ThumbEnabled, settings.ThumbQuality
	settings.ThumbEnabled = true
	settings.ThumbQuality = 80
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ThumbEnabled, settings.ThumbQuality = oldOn, oldQ
		settingsMu.Unlock()
	})

	// 复制成相对路径样张（见文件头注释）
	local := "zz_bench_src.jpg"
	raw, err := os.ReadFile(srcPhoto)
	if err != nil {
		t.Fatalf("读样张失败: %v", err)
	}
	if err := os.WriteFile(local, raw, 0o644); err != nil {
		t.Fatalf("写相对路径样张失败: %v", err)
	}
	defer os.Remove(local)

	sw, sh := 0, 0
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		sw, sh = cfg.Width, cfg.Height
	}
	t.Logf("样张 %s：%d B，%dx%d（缩略图根目录 %s）",
		filepath.Base(srcPhoto), len(raw), sw, sh, currentThumbRoot())

	var total time.Duration
	worst := time.Duration(0)
	for _, px := range []int{sizeToPixels("list"), sizeToPixels("medium"), sizeToPixels("big")} {
		out := thumbPathFor(local, px)
		_ = os.Remove(out)
		_ = os.Remove(out + thumbKeySuffix)
		_ = os.Remove(out + ".meta.json")

		t0 := time.Now()
		got, err := ensureThumb(local, "image", px)
		d := time.Since(t0)
		if err != nil {
			t.Errorf("size=%d 生成失败: %v", px, err)
			continue
		}
		total += d
		if d > worst {
			worst = d
		}
		st, _ := os.Stat(got)
		var sz int64
		if st != nil {
			sz = st.Size()
		}
		t.Logf("  size=%-5d 耗时 %8.1f ms   产物 %8d B   (%s)", px, float64(d.Microseconds())/1000, sz, filepath.ToSlash(got))
	}
	if total > 0 {
		t.Logf("三档合计 %8.1f ms（最慢一档 %.1f ms）—— 同一张原图被完整解码三次",
			float64(total.Microseconds())/1000, float64(worst.Microseconds())/1000)
		t.Logf("对比：若三档共用一次解码（或把 medium/big 映射到已有档位），这部分可省约 %.0f%%",
			float64(total-worst)/float64(total)*100)
	}
}
