package dto

// EntryPath 保存文件或目录操作的类型化路径参数。
type EntryPath struct {
	// EntryID 对应路径中的 id，范围为非零 uint64。
	EntryID uint64
}

// ParseEntryPath 解析路由已解码的 id，失败返回固定公开字段错误。
// 用户身份由认证上下文提供，目标归属及可见性由后续业务流程检查。
func ParseEntryPath(rawEntryID string) (*EntryPath, *FieldError) {
	// 1. 检查路径 ID 的范围和标准十进制写法。
	entryID, valid := parsePathID(rawEntryID)

	// 2. 对非法输入返回固定的 id 字段错误。
	if !valid {
		return nil, &FieldError{Field: "id", Message: "目录项 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 3. 返回目录项路径参数，供后续读取流程使用。
	return &EntryPath{EntryID: entryID}, nil
}
