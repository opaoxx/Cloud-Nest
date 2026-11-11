package domain

// Actor 保存通过普通请求授权检查后供业务处理使用的身份标识。
type Actor struct {
	// UserID 限定本次请求的数据所属用户。
	UserID uint64
	// SessionID 标识本次请求经过检查的会话。
	SessionID uint64
	// DeviceID 标识该会话所属设备。
	DeviceID uint64
}
