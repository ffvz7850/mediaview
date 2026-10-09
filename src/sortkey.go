package main

import "strings"

// ---------------------------------------------------------------------------
// 排序规格：字段 + 方向
//
// 为什么要有这一层：飞牛文件管理器的列头排序是「字段 id + asc/desc」两段信息，
// 字段 id 用它自己的名字（name / mtim / size / type / btim）；而本应用原来的
// sort 参数只有单段（date / name / size）且方向写死在代码里。
//
// 结果就是：在文件管理器里按「修改时间↓」看，双击第 3 张图点开大图，
// 左右切换却是按文件名升序 —— 第 3 张后面不是文件管理器的第 4 张。
// 大图预加载取的是「当前张后面的 N 张」，缩略图预生成取的是「列表前 N 张」，
// 用的都是这个错位的顺序。
//
// 所以这里把两边的词表统一成 (field, desc)，让列表顺序、大图切换顺序、
// 缩略图预生成顺序三层共用同一个来源。
// ---------------------------------------------------------------------------

// 内部字段名。与飞牛文件管理器列头的对应关系写在注释里。
const (
	sortFieldName  = "name"  // 文件名
	sortFieldMtime = "mtime" // 修改时间   ← 飞牛列 id: mtim
	sortFieldSize  = "size"  // 大小       ← 飞牛列 id: size
	sortFieldType  = "type"  // 类型       ← 飞牛列 id: type
	sortFieldBtime = "btime" // 创建时间   ← 飞牛列 id: btim
)

// sortSpec 解析后的排序规格。
type sortSpec struct {
	field string // 上面五个常量之一
	desc  bool   // true = 降序（从大到小 / 新的在前 / 名字反向）
}

// defaultSortSpec 与飞牛文件管理器的默认排序一致：文件名升序。
var defaultSortSpec = sortSpec{field: sortFieldName, desc: false}

// parseSort 解析排序参数，大小写不敏感、允许首尾空格。接受的写法：
//
//	字段 + 方向（推荐，前端与探测到飞牛偏好时统一用这个）：
//	  "name:asc" "mtim:desc" "size:desc" "type:asc" "btim:desc"
//	字段与方向用下划线连接的飞牛风格：
//	  "name_asc" "mtime_desc"
//	只给字段（不带方向）：
//	  "name" "mtim" "size" "type" "btim"
//	1.8.70 以前的旧值（保留兼容，语义不变）：
//	  "date" → 修改时间倒序（旧默认）   "name" → 文件名升序   "size" → 大小倒序
//
// 字段别名同时接受 mtime/modified/date、ctime/created/birthtime 等，
// 因为飞牛不同页面（文件列表 / 搜索 / 分享）用的词不完全一样。
//
// 无法识别时退回 defaultSortSpec —— 宁可给一个确定的顺序，也不要让列表乱序；
// 乱序会让「当前图在列表里的位置」算错，大图左右切换就会跳。
func parseSort(s string) sortSpec {
	raw := strings.ToLower(strings.TrimSpace(s))
	if raw == "" {
		return defaultSortSpec
	}
	field, dir := raw, ""
	if i := strings.IndexAny(raw, ":_"); i >= 0 {
		field, dir = raw[:i], raw[i+1:]
	}

	spec := defaultSortSpec
	switch field {
	case "name", "filename", "file_name":
		spec.field = sortFieldName
	case "mtim", "mtime", "modified", "modify_time", "modification", "date", "datetime", "time":
		spec.field = sortFieldMtime
	case "size", "filesize", "file_size":
		spec.field = sortFieldSize
	case "type", "ext", "extension", "filetype", "file_type", "kind":
		spec.field = sortFieldType
	case "btim", "btime", "ctime", "created", "create_time", "birthtime", "birth_time", "creation",
		"created_time", "createtime":
		spec.field = sortFieldBtime
	case "position", "path", "location":
		// 文件管理器有「位置」列。本应用一次只列一个目录，按路径排等价于按名字排。
		spec.field = sortFieldName
	default:
		return defaultSortSpec
	}

	switch dir {
	case "desc", "d", "down", "true", "1":
		spec.desc = true
	case "asc", "a", "up", "false", "0":
		spec.desc = false
	default:
		// 只给了字段、没给方向：沿用旧行为的默认方向，避免已保存的书签/URL 换了语义。
		// 修改时间与大小的旧默认都是倒序（date=最新在前、size=从大到小）。
		switch spec.field {
		case sortFieldMtime, sortFieldSize:
			spec.desc = true
		default:
			spec.desc = false
		}
	}
	return spec
}

// sortKey 参与排序的字段。目录与文件都有这些量（目录的 Ext 恒为空字符串）。
//
// 独立成一个小结构体而不是直接比较 FileItem/DirEntry，是为了让目录与文件
// 共用同一套比较规则 —— 飞牛文件管理器里目录也是按当前列排序的（只是恒在
// 文件前面），两边规则不一致的话，网格里目录段和文件段的顺序看着就不像
// 同一个排序。
type sortKey struct {
	Name  string
	Size  int64
	Mtime int64
	Btime int64
	Ext   string
}

// less 按规格比较两个 key；同键时用文件名自然排序兜底。
//
// 兜底不是可选项：sort.Slice 不保证稳定，而 os.ReadDir 只保证「按文件名」，
// 遇到大小/时间相同的项（连拍、批量导出、同一秒内的截图）顺序就会看起来随机，
// 同一目录两次打开顺序还不一样 —— 大图切换会莫名跳动。
func (a sortKey) less(b sortKey, sp sortSpec) bool {
	la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)

	if sp.field == sortFieldName {
		if naturalLess(la, lb) {
			return !sp.desc
		}
		if naturalLess(lb, la) {
			return sp.desc
		}
		return false
	}

	var cmp int
	switch sp.field {
	case sortFieldSize:
		cmp = compareInt64(a.Size, b.Size)
	case sortFieldBtime:
		cmp = compareInt64(a.Btime, b.Btime)
	case sortFieldType:
		cmp = strings.Compare(a.Ext, b.Ext)
	default: // 修改时间
		cmp = compareInt64(a.Mtime, b.Mtime)
	}
	if cmp == 0 {
		// 主字段相等时**不要自己兜底**：飞牛文件管理器在这种情况下返回 0，
		// 由它那边的稳定排序保持「后端返回的原始顺序」（= os.ReadDir 的字节序）。
		// 这里改用自然排序兜底的话，同一秒拍摄、批量导入的照片（mtime 完全相同）
		// 在两边的先后就会不一致 —— 大图左右切换看起来就是「顺序错乱」。
		// 配合调用方使用 sort.SliceStable：本函数返回 false 即保持原始顺序。
		return false
	}
	if sp.desc {
		return cmp > 0
	}
	return cmp < 0
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// naturalLess 自然排序：字符串里的连续数字段按**数值**比较，其余按字节序。
//
// 目的：与飞牛文件管理器保持一致。纯字典序会把 IMG_10.jpg 排在 IMG_2.jpg 前面
// （'1' < '2'），于是「文件管理器里第 2 张」和「大图里的第 2 张」不是同一张。
//
// 规则：
//   - 数字段 vs 数字段：先比有效数字位数（位数多的数值大），位数相同逐位比；
//     数值完全相同时前导零少的在前（"2" < "02"）
//   - 数字段 vs 非数字：数字在前（"2.jpg" 排在 "a.jpg" 前，与文件管理器一致）
//   - 其余：逐字节比较；一方是另一方前缀时短的在前
//
// 注意：调用方决定是否先统一大小写 —— 本应用在文件名比较处统一 ToLower，
// 因为飞牛文件管理器也是不区分大小写排的。
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		da, db := isASCIIDigit(a[i]), isASCIIDigit(b[j])
		switch {
		case da && db:
			zi, zj := i, j
			for i < len(a) && a[i] == '0' {
				i++
			}
			for j < len(b) && b[j] == '0' {
				j++
			}
			xi, xj := i, j
			for i < len(a) && isASCIIDigit(a[i]) {
				i++
			}
			for j < len(b) && isASCIIDigit(b[j]) {
				j++
			}
			na, nb := a[xi:i], b[xj:j]
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			if za, zb := xi-zi, xj-zj; za != zb {
				return za < zb
			}
		case da != db:
			return da
		default:
			if a[i] != b[j] {
				return a[i] < b[j]
			}
			i++
			j++
		}
	}
	return len(a)-i < len(b)-j
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }
