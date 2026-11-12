package dto

// LogoutRequest 保存退出请求的客户端模式。
type LogoutRequest struct {
	// ClientType 是 web 或 native，后续来源检查须与客户端请求头核对。
	ClientType string `json:"client_type"`
}

// Validate 检查客户端模式，返回固定的公开字段错误。
func (request LogoutRequest) Validate() *FieldError {
	// 1. 检查客户端类型是否为 web 或 native。
	if request.ClientType != "web" && request.ClientType != "native" {
		return &FieldError{Field: "client_type", Message: "客户端类型须为 web 或 native"}
	}

	// 2. 返回字段校验成功，供后续退出流程使用。
	return nil
}
