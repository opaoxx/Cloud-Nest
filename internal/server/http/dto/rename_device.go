package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// RenameDeviceRequest 保存修改设备名称的请求正文。
type RenameDeviceRequest struct {
	// Name 是用户提交的设备展示名称，校验成功后保留原值。
	Name string `json:"name"`
}

// Validate 检查设备名称，返回固定的公开字段错误。
func (request RenameDeviceRequest) Validate() *FieldError {
	// 1. 复用设备名称规则，检查字符长度、空白和控制字符。
	if !domain.ValidDeviceName(request.Name) {
		return &FieldError{Field: "name", Message: "设备名称须为 1～100 个字符，不能全为空白或包含控制字符"}
	}

	// 2. 返回校验成功，供后续名称更新流程使用。
	return nil
}
