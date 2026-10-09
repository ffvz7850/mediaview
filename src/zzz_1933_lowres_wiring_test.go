package main

import (
	"strings"
	"testing"
)

// TestLowresWiredIntoExtractFrame 守住「-lowres 真的接进了 ffmpeg 调用」。
//
// 背景：用户的 1.8.130 二进制里 lowres 出现 0 次 —— 这个提速杠杆完全没吃到。
func TestLowresWiredIntoExtractFrame(t *testing.T) {
	ff := stripLineComments(readSourceOrSkip(t, "ffmpeg.go"))
	if !strings.Contains(ff, "func lowresFor(") {
		t.Fatal("ffmpeg.go 里没有 lowresFor —— 档位无法动态计算")
	}
	if !strings.Contains(ff, `"-lowres"`) {
		t.Fatal("ffmpeg.go 里没有 -lowres 参数 —— 提速杠杆没接上")
	}
	// 必须是**输入选项**：-lowres 要出现在 -i 之前
	iLow := strings.Index(ff, `"-lowres"`)
	iIn := strings.Index(ff, `"-autorotate", "-i", path`)
	if iLow < 0 || iIn < 0 || iLow > iIn {
		t.Errorf("-lowres 的位置不对（lowres=%d, -i=%d）—— 它必须是输入选项，放在 -i 之前", iLow, iIn)
	}
	// 只对图片启用（视频走 -ss + VAAPI）。1.8.144 起，档位与开关判断收敛到
	// lowresForThumb()（缩略图）/ lowresForViewer()（大图）两个函数里，
	// extractFrame 只负责「拿到尺寸 → 问档位 → 加参数」。判据随之改成：
	//   - extractFrame 里必须有 `if !seek {` 图片专属分支；
	//   - 该分支里必须调用 lowresForThumb（它内部才看 ThumbLowres 总开关与档位）；
	//   - lowresForThumb 必须实现「总开关关 → 返回 0」。
	iSeek := strings.Index(ff, "if !seek {")
	if iSeek < 0 {
		t.Fatal("extractFrame 里没有 `if !seek` 分支 —— -lowres 可能对视频也生效了")
	}
	// 注意：lowresForThumb 的**定义**在文件里更靠前，所以只能从 `!seek` 分支之后再找**调用**。
	rest := ff[iSeek:]
	if !strings.Contains(rest, "lowresForThumb(") {
		t.Error("`!seek`（图片）分支里没有调用 lowresForThumb —— 缩略图档位链路断了，" +
			"或 -lowres 被加到了视频路径上")
	}
	if !strings.Contains(ff, "if !getSettings().ThumbLowres {\n\t\treturn 0") {
		t.Error("lowresForThumb 没有实现「总开关关闭 → 返回 0」")
	}
	// 设置项要存在
	cfg := stripLineComments(readSourceOrSkip(t, "config.go"))
	if !strings.Contains(cfg, "ThumbLowres") {
		t.Error("config.go 里没有 ThumbLowres 设置")
	}
}
