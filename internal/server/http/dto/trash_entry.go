package dto

// TrashEntryRequest 保存单项回收请求的版本字段。
type TrashEntryRequest struct {
	// ExpectedRevision 是客户端读到的目录项版本，缺失或 null 时为 nil。
	ExpectedRevision *string `json:"expected_revision"`
}

// TrashEntryInput 保存已校验的正文参数，目标和身份分别取自路径和认证上下文。
type TrashEntryInput struct {
	// ExpectedRevision 保存转换后的非零 uint64 版本。
	ExpectedRevision uint64
}

// ParseTrashEntryRequest 解析预期版本，实际版本比对由后续事务处理。
func ParseTrashEntryRequest(request TrashEntryRequest) (*TrashEntryInput, *FieldError) {
	// 1. 读取版本字符串，用现有规则校验格式和范围。
	var revisionText string
	if request.ExpectedRevision != nil {
		revisionText = *request.ExpectedRevision
	}
	expectedRevision, valid := parsePathID(revisionText)

	// 2. 失败时返回固定字段错误，成功时交付 uint64 版本。
	if !valid {
		return nil, &FieldError{Field: "expected_revision", Message: "预期版本须为 1～18446744073709551615 的标准十进制整数"}
	}
	return &TrashEntryInput{ExpectedRevision: expectedRevision}, nil
}
