package dto

import (
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadTaskErrorSummary 保存最近一次任务错误的公开码和固定提示。
type UploadTaskErrorSummary struct {
	// Code 是已批准的公开业务码，未知内部码统一转换为 INTERNAL_ERROR。
	Code string `json:"code"`
	// Message 是由公开码确定的固定中文提示。
	Message string `json:"message"`
}

// UploadTaskData 保存客户端查询上传任务所需的十三个公开字段。
type UploadTaskData struct {
	// ID 是完整上传任务 ID 的十进制字符串。
	ID string `json:"id"`
	// ParentID 是目标目录 ID 的十进制字符串。
	ParentID string `json:"parent_id"`
	// RequestedName 是已经校验的请求展示名。
	RequestedName string `json:"requested_name"`
	// ResolvedName 是可空的最终发布名称。
	ResolvedName *string `json:"resolved_name"`
	// SizeBytes 是 JSON 安全整数范围内的声明大小。
	SizeBytes uint64 `json:"size_bytes"`
	// UploadMode 保存 SINGLE 或 MULTIPART。
	UploadMode string `json:"upload_mode"`
	// Status 保存精确上传阶段。
	Status string `json:"status"`
	// ResultEntryID 是可空的发布结果 ID 字符串。
	ResultEntryID *string `json:"result_entry_id"`
	// CreatedAt 是 UTC 微秒创建时间。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 是 UTC 微秒更新时间。
	UpdatedAt time.Time `json:"updated_at"`
	// ExpiresAt 是 UTC 微秒到期时间，保留已到期事实。
	ExpiresAt time.Time `json:"expires_at"`
	// CompletedAt 是可空的 UTC 微秒终结时间。
	CompletedAt *time.Time `json:"completed_at"`
	// LastError 是可空的最近一次公开错误摘要。
	LastError *UploadTaskErrorSummary `json:"last_error"`
}

// UploadTaskResponse 关联上传任务查询数据与本次请求 ID。
type UploadTaskResponse struct {
	// Data 保存已转换的公开任务字段。
	Data UploadTaskData `json:"data"`
	// RequestID 与后续 HTTP 响应头及完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewUploadTaskData 转换已完成用户归属检查的任务，失败返回 nil 和错误。
func NewUploadTaskData(result *domain.UploadTask) (*UploadTaskData, error) {
	// 1. 检查任务标识、状态和公开展示字段。
	if result == nil || result.ID == 0 || result.UserID == 0 || result.TargetParentID == 0 || !result.Status.IsValid() ||
		result.RequestedName == "" || !utf8.ValidString(result.RequestedName) ||
		(result.ResolvedName != nil && (*result.ResolvedName == "" || !utf8.ValidString(*result.ResolvedName))) ||
		(result.UploadMode != "SINGLE" && result.UploadMode != "MULTIPART") {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 核对 JSON 安全整数范围及完成结果。
	if result.FileSize > maxJSONSafeInteger {
		return nil, fmt.Errorf("上传文件字节数超出 JSON 安全整数范围")
	}
	if (result.ResultEntryID != nil && *result.ResultEntryID == 0) ||
		(result.Status == domain.UploadStatusCompleted && (result.ResultEntryID == nil || result.CompletedAt == nil)) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 复用时间转换，取得独立 UTC 微秒值。
	createdAt, validCreated := listResponseTime(&result.CreatedAt)
	updatedAt, validUpdated := listResponseTime(&result.UpdatedAt)
	expiresAt, validExpires := listResponseTime(&result.ExpiresAt)
	completedAt, validCompleted := listResponseTime(result.CompletedAt)
	if !validCreated || !validUpdated || !validExpires || !validCompleted {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 4. 转换 ID，复制可空名称并生成公开错误摘要。
	data := &UploadTaskData{
		ID:            strconv.FormatUint(result.ID, 10),
		ParentID:      strconv.FormatUint(result.TargetParentID, 10),
		RequestedName: result.RequestedName,
		ResolvedName:  copyEntryText(result.ResolvedName),
		SizeBytes:     result.FileSize,
		UploadMode:    result.UploadMode,
		Status:        string(result.Status),
		CreatedAt:     *createdAt,
		UpdatedAt:     *updatedAt,
		ExpiresAt:     *expiresAt,
		CompletedAt:   completedAt,
		LastError:     uploadTaskErrorSummary(result.LastErrorCode),
	}
	if result.ResultEntryID != nil {
		entryID := strconv.FormatUint(*result.ResultEntryID, 10)
		data.ResultEntryID = &entryID
	}
	return data, nil
}

// uploadTaskErrorSummary 将内部任务错误码转换为固定的公开提示。
func uploadTaskErrorSummary(code *string) *UploadTaskErrorSummary {
	// 1. 未记录错误码时保留 null。
	if code == nil {
		return nil
	}

	// 2. 按已批准的业务码生成提示，未知码使用通用处理异常。
	publicCode := *code
	var message string
	switch publicCode {
	case domain.UploadTargetUnavailableCode:
		message = domain.ErrUploadTargetUnavailable.Error()
	case domain.UploadBodySizeMismatchCode:
		message = domain.ErrUploadBodySizeMismatch.Error()
	case "FILE_TOO_LARGE":
		message = "文件大小超过当前上传限制"
	case domain.UploadChecksumMismatchCode:
		message = domain.ErrUploadChecksumMismatch.Error()
	case domain.UploadPartsMismatchCode:
		message = domain.ErrUploadPartsMismatch.Error()
	case domain.UploadSessionLostCode:
		message = domain.ErrUploadSessionLost.Error()
	case "QUOTA_EXCEEDED":
		message = "存储空间不足"
	case "TEMP_SPACE_EXHAUSTED":
		message = "上传临时空间不足，请稍后重试"
	case "STORAGE_UNAVAILABLE":
		message = "存储服务暂时不可用"
	case "DATABASE_UNAVAILABLE":
		message = "数据服务暂时不可用"
	default:
		publicCode, message = "INTERNAL_ERROR", "上传任务处理异常"
	}
	return &UploadTaskErrorSummary{Code: publicCode, Message: message}
}
