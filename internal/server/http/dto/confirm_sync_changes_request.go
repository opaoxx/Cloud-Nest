package dto

import (
	"encoding/json"
	"errors"
)

// ConfirmSyncChangesRequest 保留增量确认游标的 JSON 原值。
type ConfirmSyncChangesRequest struct {
	cursor json.RawMessage
}

// UnmarshalJSON 将正文限制为仅可含 cursor 的单个对象。
func (request *ConfirmSyncChangesRequest) UnmarshalJSON(body []byte) error {
	// 1. 按解码后的键去重，只允许精确拼写的 cursor。
	fields, valid := readUniqueJSONObject(body)
	cursor, present := fields["cursor"]
	if !valid || len(fields) > 1 || (len(fields) == 1 && !present) {
		return errors.New("增量确认正文须为仅含 cursor 的 JSON 对象")
	}

	// 2. 保留字段原值，字段缺失交给后续游标检查。
	request.cursor = cursor
	return nil
}

// ParseCursor 校验游标文本，签名与身份绑定由确认服务核对。
func (request ConfirmSyncChangesRequest) ParseCursor() (string, bool) {
	var cursor string
	if err := json.Unmarshal(request.cursor, &cursor); err != nil || !validSyncSnapshotTokenText(cursor) {
		return "", false
	}
	return cursor, true
}
