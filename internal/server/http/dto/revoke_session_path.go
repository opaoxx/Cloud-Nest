package dto

// RevokeSessionPath 保存撤销指定会话的类型化路径参数。
type RevokeSessionPath struct {
	// SessionID 对应路径中的 id，范围为非零 uint64。
	SessionID uint64
}

// ParseRevokeSessionPath 解析路由已解码的 id，失败返回固定公开字段错误。
// 参数只表达目标会话，用户身份由认证上下文提供，归属由后续业务流程检查。
func ParseRevokeSessionPath(rawSessionID string) (*RevokeSessionPath, *FieldError) {
	// 1. 检查路径 ID 的范围和标准十进制写法。
	sessionID, valid := parsePathID(rawSessionID)

	// 2. 检查非零和标准十进制写法，失败返回固定字段错误。
	if !valid {
		return nil, &FieldError{Field: "id", Message: "会话 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 3. 返回供后续撤销服务使用的类型化参数。
	return &RevokeSessionPath{SessionID: sessionID}, nil
}
