package dto

import "strconv"

// parsePathID 校验已解码的资源 ID，返回非零 uint64 及合法标记。
func parsePathID(rawID string) (uint64, bool) {
	// 1. 转换为 uint64，检查非零和标准十进制写法。
	identifier, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || identifier == 0 || strconv.FormatUint(identifier, 10) != rawID {
		return 0, false
	}
	return identifier, true
}
