package dto

// MoveEntryRequest 保存目录项移动的 JSON 请求字段。
type MoveEntryRequest struct {
	// TargetParentID 是目标父目录的非零无符号标识字符串。
	TargetParentID string `json:"target_parent_id"`
	// ExpectedRevision 是客户端读取源目录项时看到的版本字符串。
	ExpectedRevision string `json:"expected_revision"`
}

// MoveEntryInput 保存移动参数，源目录项来自路径，身份来自认证上下文。
type MoveEntryInput struct {
	// TargetParentID 是转换后的目标父目录标识。
	TargetParentID uint64
	// ExpectedRevision 是转换后的客户端预期版本。
	ExpectedRevision uint64
}

// ParseMoveEntryRequest 按目标父目录、预期版本的顺序解析移动正文。
// 失败返回 nil 和固定字段错误，目录关系和版本比对由后续事务服务处理。
func ParseMoveEntryRequest(request MoveEntryRequest) (*MoveEntryInput, *FieldError) {
	// 1. 校验并转换目标父目录 ID。
	targetParentID, validParent := parsePathID(request.TargetParentID)
	if !validParent {
		return nil, &FieldError{Field: "target_parent_id", Message: "目标父目录 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 2. 校验并转换预期版本。
	expectedRevision, validRevision := parsePathID(request.ExpectedRevision)
	if !validRevision {
		return nil, &FieldError{Field: "expected_revision", Message: "预期版本须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 3. 返回独立的结构化移动参数。
	return &MoveEntryInput{TargetParentID: targetParentID, ExpectedRevision: expectedRevision}, nil
}
