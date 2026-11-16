package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// ValidateCreateUploadInput 校验上传创建声明，返回展示名有效且摘要独立的输入。
// 校验按目录标识、完整名称、声明大小的顺序进行，失败返回 nil 和领域错误。
func ValidateCreateUploadInput(input domain.CreateUploadInput) (*domain.CreateUploadInput, error) {
	// 1. 检查上传目标目录 ID。
	if input.ParentID == 0 {
		return nil, domain.ErrInvalidUploadParentID
	}

	// 2. 复用统一命名规则，取得有效展示名。
	name, err := ValidateEntryName(input.Name)
	if err != nil {
		return nil, err
	}

	// 3. 检查首期 SINGLE 上传的 16 MiB 上限。
	if input.SizeBytes > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}

	// 4. 复制预期摘要，交付独立的规范化输入。
	prepared := input
	prepared.Name = name
	prepared.ExpectedSHA256 = copyOptionalValue(input.ExpectedSHA256)
	return &prepared, nil
}
