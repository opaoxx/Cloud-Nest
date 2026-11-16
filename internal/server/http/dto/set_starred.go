package dto

// SetStarredRequest 保存设置目录项收藏状态的 JSON 请求字段。
type SetStarredRequest struct {
	// Starred 用指针区分显式 false 与字段缺失或 null。
	Starred *bool `json:"starred"`
}

// SetStarredInput 保存明确的目标收藏状态，身份来自认证上下文，目录项 ID 来自路径。
type SetStarredInput struct {
	// Starred 为 true 时表示收藏，为 false 时表示取消收藏。
	Starred bool
}

// ParseSetStarredRequest 校验显式收藏状态，失败返回空结果与固定字段错误。
func ParseSetStarredRequest(request SetStarredRequest) (*SetStarredInput, *FieldError) {
	// 1. 检查是否明确提供收藏状态。
	if request.Starred == nil {
		return nil, &FieldError{Field: "starred", Message: "收藏状态须明确为 true 或 false"}
	}

	// 2. 复制布尔值，返回结构化收藏参数。
	return &SetStarredInput{Starred: *request.Starred}, nil
}
