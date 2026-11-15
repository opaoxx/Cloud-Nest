package dto

import (
	"strings"
	"unicode/utf8"
)

// CreateDirectoryRequest 保存创建文件夹的 JSON 请求字段。
type CreateDirectoryRequest struct {
	// ParentID 是客户端传入的非零 uint64 标准十进制字符串。
	ParentID string `json:"parent_id"`
	// Name 是原始名称，完整命名规则由后续服务复用统一校验。
	Name string `json:"name"`
	// ColorKey 省略或 null 时为 nil，显式空字符串保留独立含义。
	ColorKey *string `json:"color_key"`
}

// CreateDirectoryInput 保存请求解析后的父目录 ID、原始名称和独立颜色值。
type CreateDirectoryInput struct {
	// ParentID 是转换后的真实父目录 ID。
	ParentID uint64
	// Name 保留用户输入，供后续名称校验和名称键生成使用。
	Name string
	// ColorKey 为 nil 时采用默认配色语义，否则保留原字符串。
	ColorKey *string
}

// ParseCreateDirectoryRequest 按父目录、名称、颜色顺序解析请求。
// 失败返回 nil 和固定字段错误，成功返回独立输入，用户身份由认证上下文提供。
func ParseCreateDirectoryRequest(request CreateDirectoryRequest) (*CreateDirectoryInput, *FieldError) {
	// 1. 校验并转换必填的父目录 ID。
	parentID, valid := parsePathID(request.ParentID)
	if !valid {
		return nil, &FieldError{Field: "parent_id", Message: "父目录 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 2. 检查名称是否为非空有效 UTF-8 文本。
	if request.Name == "" || !utf8.ValidString(request.Name) {
		return nil, &FieldError{Field: "name", Message: "名称须为非空的有效 UTF-8 文本"}
	}

	// 3. 检查可空颜色键的长度和字符范围。
	if request.ColorKey != nil && (len(*request.ColorKey) > 16 || strings.ContainsFunc(*request.ColorKey, func(character rune) bool {
		return character < 0x20 || character > 0x7e
	})) {
		return nil, &FieldError{Field: "color_key", Message: "颜色键须为 null 或不超过 16 字节的可打印 ASCII 文本"}
	}

	// 4. 复制颜色值，返回结构化输入。
	input := &CreateDirectoryInput{ParentID: parentID, Name: request.Name}
	if request.ColorKey != nil {
		colorCopy := *request.ColorKey
		input.ColorKey = &colorCopy
	}
	return input, nil
}
