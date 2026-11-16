package dto

// RestoreRenameEntryData 恢复幂等记录中的稳定重命名快照。
// 结果独立持有字段，时间沿用 UTC 微秒规则，异常统一返回幂等记录数据错误。
func RestoreRenameEntryData(body []byte) (*RenameEntryData, error) {
	// 1. 复用普通目录项快照的完整校验。
	entry, err := restoreEntrySnapshot(body)
	if err != nil {
		return nil, err
	}

	// 2. 组装独立的重命名结果。
	return &RenameEntryData{Entry: *entry}, nil
}
