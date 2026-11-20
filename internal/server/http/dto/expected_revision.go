package dto

// parseExpectedRevision 校验回收和恢复正文中的可空版本字段。
func parseExpectedRevision(value *string) (uint64, *FieldError) {
	// 1. 读取版本字符串，校验非零十进制格式和 uint64 范围。
	var revisionText string
	if value != nil {
		revisionText = *value
	}
	revision, valid := parsePathID(revisionText)

	// 2. 错误时返回固定字段提示，成功时交付整数版本。
	if !valid {
		return 0, &FieldError{Field: "expected_revision", Message: "预期版本须为 1～18446744073709551615 的标准十进制整数"}
	}
	return revision, nil
}
