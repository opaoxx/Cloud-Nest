package dto

import (
	"net/url"

	"github.com/google/uuid"
)

// SyncSnapshotPageRequest 保存类型化路径和解码后的续页凭据。
type SyncSnapshotPageRequest struct {
	SnapshotID uuid.UUID
	Cursor     string
}

// ParseSyncSnapshotPageRequest 解析已解码的路径参数和完整 RawQuery。
// 签名和身份绑定由续页服务核对。
func ParseSyncSnapshotPageRequest(rawSnapshotID, rawQuery string) (*SyncSnapshotPageRequest, *FieldError) {
	// 1. 校验快照 ID 的 UUID 版本、变体和规范小写写法。
	snapshotID, fieldError := ParseSyncSnapshotID(rawSnapshotID)
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 完整解析查询，确保 cursor 恰好出现一次。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	cursors := values["cursor"]
	if len(cursors) != 1 || !validSyncSnapshotTokenText(cursors[0]) {
		return nil, &FieldError{Field: "cursor", Message: "快照续页凭据须为单个有效格式的文本"}
	}

	// 3. 保留解码后的凭据原文，供后续按原文本验签。
	return &SyncSnapshotPageRequest{SnapshotID: snapshotID, Cursor: cursors[0]}, nil
}

// ParseSyncSnapshotID 将已解码的路径参数转换为标准小写 UUID v4。
func ParseSyncSnapshotID(rawSnapshotID string) (uuid.UUID, *FieldError) {
	snapshotID, err := uuid.Parse(rawSnapshotID)
	if err != nil || snapshotID.Version() != 4 || snapshotID.Variant() != uuid.RFC4122 || snapshotID.String() != rawSnapshotID {
		return uuid.Nil, &FieldError{Field: "snapshot_id", Message: "快照 ID 须为标准小写 UUID v4"}
	}
	return snapshotID, nil
}
