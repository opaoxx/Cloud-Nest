package dto

// UploadTaskPath 保存上传任务查询的类型化路径参数。
type UploadTaskPath struct {
	// TaskID 对应路径中的 id，范围为非零 uint64。
	TaskID uint64
}

// ParseUploadTaskPath 解析路由已解码的 id，失败返回固定公开字段错误。
// 用户身份由认证上下文提供，任务归属由后续业务流程检查。
func ParseUploadTaskPath(rawTaskID string) (*UploadTaskPath, *FieldError) {
	// 1. 检查路径 ID 的范围和标准十进制写法。
	taskID, valid := parsePathID(rawTaskID)

	// 2. 对非法输入返回固定的 id 字段错误。
	if !valid {
		return nil, &FieldError{Field: "id", Message: "上传任务 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 3. 返回上传任务路径参数，供后续查询流程使用。
	return &UploadTaskPath{TaskID: taskID}, nil
}
