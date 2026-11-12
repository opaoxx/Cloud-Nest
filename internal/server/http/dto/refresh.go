package dto

// RefreshRequest 保存刷新请求的客户端模式和原生刷新原文。
type RefreshRequest struct {
	// ClientType 是 web 或 native，后续来源检查须与客户端请求头核对。
	ClientType string `json:"client_type"`
	// RefreshToken 是原生客户端提交的原文，Web 的正文值为空，凭证由 Cookie 提供。
	RefreshToken string `json:"refresh_token,omitempty"`
}

// Validate 检查客户端模式与凭证字段组合，返回第一个固定字段错误。
// 凭证原文由后续刷新服务检查格式、摘要和会话状态。
func (request RefreshRequest) Validate() *FieldError {
	// 1. 检查客户端类型是否为 web 或 native。
	if request.ClientType != "web" && request.ClientType != "native" {
		return &FieldError{Field: "client_type", Message: "客户端类型须为 web 或 native"}
	}

	// 2. 检查 Web 正文中的刷新凭证字段。
	if request.ClientType == "web" && request.RefreshToken != "" {
		return &FieldError{Field: "refresh_token", Message: "web 客户端须通过刷新 Cookie 提交凭证"}
	}

	// 3. 检查原生客户端是否提供刷新原文。
	if request.ClientType == "native" && request.RefreshToken == "" {
		return &FieldError{Field: "refresh_token", Message: "原生客户端须提供刷新凭证"}
	}
	return nil
}
