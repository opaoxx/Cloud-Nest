package dto

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EncodeCreateUploadReplacementSnapshot 保存内容替换首次创建的公开 data，供幂等重放。
func EncodeCreateUploadReplacementSnapshot(task *domain.UploadTask) (json.RawMessage, error) {
	// 1. 核对替换绑定及首次创建的任务事实。
	if task == nil || task.ReplacementEntryID == nil || !task.HasValidReplacementState() ||
		task.StorageUploadID != nil || task.PublishedObjectID != nil || task.LastErrorMessage != nil ||
		task.LeaseOwner != nil || task.LeaseUntil != nil || task.LeaseVersion != 0 || task.NextAttemptAt != nil || task.AttemptCount != 0 ||
		(task.UploadMode == "SINGLE" && (task.PartSize != nil || task.PartCount != nil)) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 按模式复用公开任务、内容路径或固定分片计划的编码。
	return EncodeCreateUploadSnapshot(task)
}

// RestoreCreateUploadReplacementData 恢复含原目标绑定的 SINGLE 创建快照。
func RestoreCreateUploadReplacementData(body []byte) (*CreateUploadData, error) {
	return restoreCreateUploadData(body, true)
}

// RestoreCreateMultipartUploadReplacementData 恢复含原目标绑定的 MULTIPART 创建快照。
func RestoreCreateMultipartUploadReplacementData(body []byte) (*CreateMultipartUploadData, error) {
	return restoreCreateMultipartUploadData(body, true)
}

// ValidateCreateUploadReplacementSnapshot 核对回执快照与保存的任务 ID 及本次替换请求一致。
func ValidateCreateUploadReplacementSnapshot(body []byte, taskID, entryID, expectedRevision, sizeBytes uint64) error {
	// 1. 根据原声明的字节数选择恢复方法，核对该模式的完整快照。
	var task UploadTaskData
	if sizeBytes <= domain.MaxSingleUploadSizeBytes {
		data, err := RestoreCreateUploadReplacementData(body)
		if err != nil {
			return err
		}
		task = data.Task
	} else {
		data, err := RestoreCreateMultipartUploadReplacementData(body)
		if err != nil {
			return err
		}
		task = data.Task
	}

	// 2. 对照回执任务及原目标、版本和字节数，保留首次保存的计划。
	if task.ID != strconv.FormatUint(taskID, 10) || task.SizeBytes != sizeBytes || task.Replacement == nil ||
		task.Replacement.EntryID != strconv.FormatUint(entryID, 10) ||
		task.Replacement.ExpectedRevision != strconv.FormatUint(expectedRevision, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}

func validCreateUploadReplacementBinding(raw []byte) bool {
	// 1. 绑定恰有四个字段，显式记录两个尚未产生的结果。
	fields, valid := readUniqueJSONObject(raw)
	if !valid || len(fields) != 4 ||
		!bytes.Equal(bytes.TrimSpace(fields["published_revision"]), []byte("null")) ||
		!bytes.Equal(bytes.TrimSpace(fields["conflicting_revision"]), []byte("null")) {
		return false
	}

	// 2. 目标 ID 和预期版本必须是非零 uint64 的标准十进制字符串。
	var entryID, revision string
	if json.Unmarshal(fields["entry_id"], &entryID) != nil || json.Unmarshal(fields["expected_revision"], &revision) != nil {
		return false
	}
	_, validEntryID := parsePathID(entryID)
	_, validRevision := parsePathID(revision)
	return validEntryID && validRevision
}
