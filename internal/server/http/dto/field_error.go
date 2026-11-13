package dto

// FieldError 仅保存固定的公开字段名和校验提示，禁止填入原始输入或底层错误。
type FieldError struct {
	// Field 是公开请求字段、查询或路径参数名称，用于客户端定位输入错误。
	Field string
	// Message 是可公开的规则说明，不包含字段的实际值。
	Message string
}
