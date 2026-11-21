package service

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MaxUploadPartSigningInputs 是一次分片签发请求允许声明的最大项数。
const MaxUploadPartSigningInputs = domain.MaxUploadPartSigningInputs

// UploadPartDeclaration 保存客户端声明的编号和分片 SHA-256 十六进制文本。
type UploadPartDeclaration = domain.UploadPartDeclaration

// UploadPartSigningInput 保存核对后的编号、预期长度及签名需要的校验头值。
type UploadPartSigningInput struct {
	PartNumber     uint32
	SizeBytes      uint64
	ChecksumSHA256 string // 标准 Base64，保留填充字符。
}

// UploadPartDeclarationError 携带可定位到批次或单项的输入错误。
type UploadPartDeclarationError struct {
	Field   string
	Message string
}

func (inputError *UploadPartDeclarationError) Error() string {
	return inputError.Message
}

// PrepareUploadPartSigningInputs 根据已保存的计划校验声明，按输入顺序返回完整批次。
func PrepareUploadPartSigningInputs(fileSizeBytes, partSizeBytes uint64, partCount uint32, parts []UploadPartDeclaration) ([]UploadPartSigningInput, error) {
	// 1. 检查批次数量，准备记录已经出现的分片编号。
	if len(parts) < 1 || len(parts) > MaxUploadPartSigningInputs {
		return nil, &UploadPartDeclarationError{Field: "parts", Message: fmt.Sprintf("分片声明须包含 1～%d 项", MaxUploadPartSigningInputs)}
	}
	seen := make(map[uint32]struct{}, len(parts))
	signingInputs := make([]UploadPartSigningInput, 0, len(parts))
	for index, part := range parts {
		// 2. 用任务固定计划计算长度，区分持久化计划异常和客户端编号错误。
		sizeBytes, err := domain.ExpectedMultipartUploadPartSize(fileSizeBytes, partSizeBytes, partCount, part.PartNumber)
		if errors.Is(err, domain.ErrInvalidUploadPartNumber) {
			return nil, &UploadPartDeclarationError{Field: fmt.Sprintf("parts.%d.part_number", index), Message: err.Error()}
		}
		if err != nil {
			return nil, err
		}
		if _, exists := seen[part.PartNumber]; exists {
			return nil, &UploadPartDeclarationError{Field: "parts", Message: "同批分片编号不得重复"}
		}
		seen[part.PartNumber] = struct{}{}

		// 3. 解码固定长度的十六进制摘要，供签名和请求头使用同一标准 Base64 值。
		digest, valid := domain.DecodeUploadPartSHA256(part.SHA256)
		if !valid {
			return nil, &UploadPartDeclarationError{Field: fmt.Sprintf("parts.%d.sha256", index), Message: "SHA-256 须为 64 位十六进制文本"}
		}
		signingInputs = append(signingInputs, UploadPartSigningInput{
			PartNumber: part.PartNumber, SizeBytes: sizeBytes, ChecksumSHA256: base64.StdEncoding.EncodeToString(digest[:]),
		})
	}
	return signingInputs, nil
}
