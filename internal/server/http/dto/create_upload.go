package dto

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateUploadRequest 保存单文件上传创建的 JSON 声明。
type CreateUploadRequest struct {
	// ParentID 为目标目录的非零 uint64 标准十进制字符串。
	ParentID string `json:"parent_id"`
	// Name 保留原始文件名，后续服务复用完整命名规则。
	Name string `json:"name"`
	// SizeBytes 区分必填声明缺失与显式零字节文件。
	SizeBytes *uint64 `json:"size_bytes"`
	// SHA256 可省略或为 null，非空时使用 64 位十六进制文本。
	SHA256 *string `json:"sha256"`
}

// ParseCreateUploadRequest 按固定字段顺序转换声明，成功返回独立输入。
// 声明大小原值由领域模型和上传服务核对能力上限，名称保留原文供统一校验。
func ParseCreateUploadRequest(request CreateUploadRequest) (*domain.CreateUploadInput, *FieldError) {
	// 1. 校验并转换目标目录 ID。
	parentID, valid := parsePathID(request.ParentID)
	if !valid {
		return nil, &FieldError{Field: "parent_id", Message: "父目录 ID 须为 1～18446744073709551615 的标准十进制整数"}
	}

	// 2. 检查原始文件名的非空和 UTF-8 编码。
	if request.Name == "" || !utf8.ValidString(request.Name) {
		return nil, &FieldError{Field: "name", Message: "名称须为非空的有效 UTF-8 文本"}
	}

	// 3. 检查声明大小是否明确提供，保留完整数值。
	if request.SizeBytes == nil {
		return nil, &FieldError{Field: "size_bytes", Message: "文件大小须为必填的非负整数"}
	}

	// 4. 校验可空 SHA-256 文本并复制为固定摘要。
	var expectedSHA256 *[sha256.Size]byte
	if request.SHA256 != nil {
		if len(*request.SHA256) != hex.EncodedLen(sha256.Size) {
			return nil, &FieldError{Field: "sha256", Message: "SHA-256 须为 null 或 64 位十六进制文本"}
		}
		decoded, err := hex.DecodeString(*request.SHA256)
		if err != nil {
			return nil, &FieldError{Field: "sha256", Message: "SHA-256 须为 null 或 64 位十六进制文本"}
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		expectedSHA256 = &digest
	}

	// 5. 返回独立的上传创建输入。
	return &domain.CreateUploadInput{
		ParentID: parentID, Name: request.Name, SizeBytes: *request.SizeBytes, ExpectedSHA256: expectedSHA256,
	}, nil
}
