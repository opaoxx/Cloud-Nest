package dto

// RestoreEntryRequest 保存单项恢复请求的版本字段。
type RestoreEntryRequest struct {
	// ExpectedRevision 是客户端读到的回收项版本，缺失或 null 时为 nil。
	ExpectedRevision *string `json:"expected_revision"`
}

// RestoreEntryInput 保存已校验的恢复正文参数。
type RestoreEntryInput struct {
	// ExpectedRevision 是用于持锁版本比对的非零 uint64 版本。
	ExpectedRevision uint64
}

// ParseRestoreEntryRequest 解析单项恢复请求的预期版本。
func ParseRestoreEntryRequest(request RestoreEntryRequest) (*RestoreEntryInput, *FieldError) {
	// 1. 复用目录项的预期版本校验。
	revision, fieldError := parseExpectedRevision(request.ExpectedRevision)
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 返回后续事务使用的整数版本。
	return &RestoreEntryInput{ExpectedRevision: revision}, nil
}
