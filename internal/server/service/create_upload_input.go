package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// ValidateCreateUploadInput 校验上传创建声明，返回展示名有效且摘要独立的输入。
// 校验按目录标识、完整名称、声明大小的顺序进行，失败返回 nil 和领域错误。
func ValidateCreateUploadInput(input domain.CreateUploadInput) (*domain.CreateUploadInput, error) {
	// 1. 检查上传目标目录 ID 和文件名。
	name, err := validateCreateUploadTarget(input)
	if err != nil {
		return nil, err
	}

	// 2. 检查首期 SINGLE 上传的 16 MiB 上限。
	if input.SizeBytes > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}

	// 3. 复制预期摘要，交付独立的规范化输入。
	input.Name = name
	input.ExpectedSHA256 = copyOptionalValue(input.ExpectedSHA256)
	return &input, nil
}

// ValidateCreateUploadInputWithPlan 校验 B4 上传声明，同时交付独立输入和固定计划。
// 失败时保留对应领域错误，并返回 nil 声明和零值计划。
func ValidateCreateUploadInputWithPlan(input domain.CreateUploadInput, limits domain.UploadPlanLimits) (*domain.CreateUploadInput, domain.UploadPlan, error) {
	// 1. 检查父目录 ID，按既有规则校验文件名。
	name, err := validateCreateUploadTarget(input)
	if err != nil {
		return nil, domain.UploadPlan{}, err
	}

	// 2. 用声明大小计算计划，超限时返回实际大小上限。
	plan, err := domain.PlanUpload(input.SizeBytes, limits)
	if err != nil {
		return nil, domain.UploadPlan{}, err
	}

	// 3. Multipart 必须提供整文件 SHA-256，校验后复制摘要并返回结果。
	if plan.Mode == "MULTIPART" && input.ExpectedSHA256 == nil {
		return nil, domain.UploadPlan{}, domain.ErrMultipartUploadSHA256Required
	}
	input.Name = name
	input.ExpectedSHA256 = copyOptionalValue(input.ExpectedSHA256)
	return &input, plan, nil
}

func validateCreateUploadTarget(input domain.CreateUploadInput) (string, error) {
	if input.ParentID == 0 {
		return "", domain.ErrInvalidUploadParentID
	}
	return ValidateEntryName(input.Name)
}
