package dto

import (
	"encoding/json"
	"errors"
)

// ConfirmSyncSnapshotBaselineRequest 保留基线凭据的 JSON 原值，供字段校验使用。
type ConfirmSyncSnapshotBaselineRequest struct {
	baselineToken json.RawMessage
}

// UnmarshalJSON 将正文限制为仅可含 baseline_token 的单个对象。
func (request *ConfirmSyncSnapshotBaselineRequest) UnmarshalJSON(body []byte) error {
	// 1. 按解码后的键去重，只允许精确拼写的 baseline_token。
	fields, valid := readUniqueJSONObject(body)
	baselineToken, present := fields["baseline_token"]
	if !valid || len(fields) > 1 || (len(fields) == 1 && !present) {
		return errors.New("基线确认正文须为仅含 baseline_token 的 JSON 对象")
	}

	// 2. 保留字段原值，字段缺失时交给凭据校验交付专用错误。
	request.baselineToken = baselineToken
	return nil
}

// ParseBaselineToken 读取凭据文本并校验格式。签名、用途和身份绑定由确认服务核对。
func (request ConfirmSyncSnapshotBaselineRequest) ParseBaselineToken() (string, bool) {
	var token string
	if err := json.Unmarshal(request.baselineToken, &token); err != nil || !validSyncSnapshotTokenText(token) {
		return "", false
	}
	return token, true
}
