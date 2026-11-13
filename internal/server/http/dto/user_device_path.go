package dto

// UserDevicePath 保存设备操作的类型化路径参数。
type UserDevicePath struct {
	// DeviceID 对应路径中的 id，范围为非零 uint64。
	DeviceID uint64
}

// ParseUserDevicePath 解析路由已解码的 id，失败返回固定公开字段错误。
// 用户身份由认证上下文提供，目标设备归属由后续业务流程检查。
func ParseUserDevicePath(rawDeviceID string) (*UserDevicePath, *FieldError) {
	// 1. 检查路径 ID 的范围和标准十进制写法。
	deviceID, valid := parsePathID(rawDeviceID)

	// 2. 对非法输入返回固定的 id 字段错误。
	if !valid {
		return nil, &FieldError{Field: "id", Message: "设备 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 3. 返回设备路径参数，供后续更新流程使用。
	return &UserDevicePath{DeviceID: deviceID}, nil
}
