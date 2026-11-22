package dto

import (
	"bytes"
	"encoding/json"
	"io"

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

func validCreateUploadReplacementBinding(raw []byte) bool {
	// 1. 绑定恰有四个字段，显式记录两个尚未产生的结果。
	fields, valid := readUniqueUploadSnapshotObject(raw)
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

// readUniqueUploadSnapshotObject 读取一个 JSON 对象，并拒绝重复字段和追加内容。
func readUniqueUploadSnapshotObject(body []byte) (map[string]json.RawMessage, bool) {
	// 1. 解码对象，并在键解码后检查重复，覆盖转义写法。
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, isString := token.(string)
		if _, duplicate := fields[key]; err != nil || !isString || duplicate {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		fields[key] = value
	}

	// 2. 确认对象闭合且正文到此结束。
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return fields, true
}
