package dto

import (
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateMultipartUploadData 是创建时交付并保存到幂等记录的公开快照。
type CreateMultipartUploadData struct {
	Task          UploadTaskData `json:"task"`
	UploadMode    string         `json:"upload_mode"`
	PartSizeBytes uint64         `json:"part_size_bytes"`
	PartCount     uint32         `json:"part_count"`
	ExpiresAt     time.Time      `json:"expires_at"`
}

// NewCreateMultipartUploadData 从首次创建的任务取得公开快照，失败返回领域错误。
func NewCreateMultipartUploadData(result *domain.UploadTask) (*CreateMultipartUploadData, error) {
	// 1. 核对初始任务、声明摘要和已保存的分片计划。
	if result == nil || result.DeviceID == 0 || result.StagedObjectID == 0 ||
		result.UploadMode != "MULTIPART" || result.Status != domain.UploadStatusCreating ||
		result.ExpectedSHA256 == nil || result.ReservedBytes != result.FileSize || result.StorageUploadID != nil ||
		result.PublishedObjectID != nil || result.ResolvedName != nil || result.ResultEntryID != nil ||
		result.CompletedAt != nil || result.LastErrorCode != nil || result.LastErrorMessage != nil ||
		result.PartSize == nil || result.PartCount == nil ||
		!validMultipartSnapshotLayout(result.FileSize, *result.PartSize, *result.PartCount) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 复用任务快照转换，再复制公开计划和同一个截止时间。
	task, err := NewUploadTaskData(result)
	if err != nil {
		return nil, err
	}
	return &CreateMultipartUploadData{
		Task: *task, UploadMode: task.UploadMode, PartSizeBytes: *result.PartSize,
		PartCount: *result.PartCount, ExpiresAt: task.ExpiresAt,
	}, nil
}

// RestoreCreateMultipartUploadData 核对幂等记录中的 Multipart 创建快照及其固定计划。
func RestoreCreateMultipartUploadData(body []byte) (*CreateMultipartUploadData, error) {
	return restoreCreateMultipartUploadData(body, false)
}

func restoreCreateMultipartUploadData(body []byte, requireReplacement bool) (*CreateMultipartUploadData, error) {
	// 1. 核对外层五个字段、公开任务字段和初始化空值。
	if !hasCreateUploadSnapshotFields(body, requireReplacement, "task", "upload_mode", "part_size_bytes", "part_count", "expires_at") {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 解码并核对模式、任务阶段、标识和分片计划。
	var data CreateMultipartUploadData
	if json.Unmarshal(body, &data) != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	task := &data.Task
	_, validID := parsePathID(task.ID)
	_, validParent := parsePathID(task.ParentID)
	if !validID || !validParent || task.RequestedName == "" || !utf8.ValidString(task.RequestedName) ||
		task.UploadMode != "MULTIPART" || data.UploadMode != task.UploadMode || task.Status != string(domain.UploadStatusCreating) ||
		!validMultipartSnapshotLayout(task.SizeBytes, data.PartSizeBytes, data.PartCount) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 规范化时间并保留创建时的到期事实，供幂等请求交付。
	if !normalizeCreateUploadSnapshotTimes(task, &data.ExpiresAt) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return &data, nil
}

func validMultipartSnapshotLayout(sizeBytes, partSizeBytes uint64, partCount uint32) bool {
	return sizeBytes <= maxJSONSafeInteger && partSizeBytes <= maxJSONSafeInteger &&
		domain.ValidMultipartUploadLayout(sizeBytes, partSizeBytes, partCount)
}
