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
	// 1. 复用目录项的预期版本校验。
	expectedRevision, fieldError := parseExpectedRevision(request.ExpectedRevision)
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 返回后续事务使用的整数版本。
	return &TrashEntryInput{ExpectedRevision: expectedRevision}, nil
}
