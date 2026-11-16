package dto

// RestoreMoveEntryData 恢复幂等记录中的稳定移动快照。
// 结果独立持有字段，时间沿用 UTC 微秒规则，异常统一返回幂等记录数据错误。
func RestoreMoveEntryData(body []byte) (*MoveEntryData, error) {
	// 1. 复用普通目录项快照的完整校验。
	entry, err := restoreEntrySnapshot(body)
	if err != nil {
		return nil, err
	}

	// 2. 组装独立的移动结果。
	return &MoveEntryData{Entry: *entry}, nil
}
