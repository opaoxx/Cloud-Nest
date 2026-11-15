package dto

import "unicode/utf8"

// RenameEntryRequest 保存目录项重命名的 JSON 请求字段。
type RenameEntryRequest struct {
	// Name 是原始新名称，完整命名规则由后续服务复用统一校验。
	Name string `json:"name"`
	// ExpectedRevision 是客户端读取目录项时看到的非零版本字符串。
	ExpectedRevision string `json:"expected_revision"`
}

// RenameEntryInput 保存重命名参数，目标 ID 来自路径，用户身份来自认证上下文。
type RenameEntryInput struct {
	// Name 保留用户原始输入，供后续统一命名规则处理。
	Name string
	// ExpectedRevision 是转换后的客户端预期版本。
	ExpectedRevision uint64
}

// ParseRenameEntryRequest 按名称、预期版本顺序解析重命名正文。
// 失败返回 nil 和固定字段错误，版本比对由后续持锁事务中的服务处理。
func ParseRenameEntryRequest(request RenameEntryRequest) (*RenameEntryInput, *FieldError) {
	// 1. 检查新名称是否为非空有效 UTF-8 文本。
	if request.Name == "" || !utf8.ValidString(request.Name) {
		return nil, &FieldError{Field: "name", Message: "名称须为非空的有效 UTF-8 文本"}
	}

	// 2. 校验并转换预期版本。
	expectedRevision, valid := parsePathID(request.ExpectedRevision)
	if !valid {
		return nil, &FieldError{Field: "expected_revision", Message: "预期版本须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 3. 返回保留名称原文的结构化输入。
	return &RenameEntryInput{Name: request.Name, ExpectedRevision: expectedRevision}, nil
}
