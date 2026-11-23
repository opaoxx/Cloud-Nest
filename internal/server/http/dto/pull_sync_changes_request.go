package dto

import (
	"encoding/json"
	"errors"
)

// PullSyncChangesRequest 保存正文的原始字段，区分省略与显式空值。
type PullSyncChangesRequest struct {
	baselineToken json.RawMessage
	cursor        json.RawMessage
	pageSize      json.RawMessage
}

// PullSyncChangesInput 是解析后的拉取参数，两个凭据字段恰有一个非空。
type PullSyncChangesInput struct {
	BaselineToken string
	Cursor        string
	PageSize      int
}

// UnmarshalJSON 仅接受一种凭据和可选的 page_size，并检查字段唯一性。
func (request *PullSyncChangesRequest) UnmarshalJSON(body []byte) error {
	// 1. 按解码后的键去重，并核对凭据二选一及可选字段的精确拼写。
	fields, valid := readUniqueJSONObject(body)
	baselineToken, hasBaselineToken := fields["baseline_token"]
	cursor, hasCursor := fields["cursor"]
	pageSize, hasPageSize := fields["page_size"]
	if !valid || hasBaselineToken == hasCursor || len(fields) > 2 || (len(fields) == 2 && !hasPageSize) {
		return errors.New("增量拉取正文须恰有一种凭据，且仅可另含 page_size")
	}

	// 2. 保留合法组合的原始 JSON 值，供字段级校验使用。
	*request = PullSyncChangesRequest{baselineToken: baselineToken, cursor: cursor, pageSize: pageSize}
	return nil
}

// ParsePullSyncChangesInput 校验已绑定的页大小和凭据文本，不改变凭据原文。
func ParsePullSyncChangesInput(request PullSyncChangesRequest) (*PullSyncChangesInput, *FieldError) {
	// 1. 检查页大小。显式空值和超出 1～100 的值均拒绝。
	pageSize, fieldError := parseSyncPageSize(request.pageSize)
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 两种凭据沿用相同的外层文本格式，签名用途由后续服务校验。
	field, rawToken := "cursor", request.cursor
	if request.baselineToken != nil {
		field, rawToken = "baseline_token", request.baselineToken
	}
	var token string
	if err := json.Unmarshal(rawToken, &token); err != nil || !validSyncSnapshotTokenText(token) {
		return nil, &FieldError{Field: field, Message: "增量拉取凭据格式无效"}
	}

	// 3. 保留对应凭据的解码原文，供后续验签。
	if field == "baseline_token" {
		return &PullSyncChangesInput{BaselineToken: token, PageSize: pageSize}, nil
	}
	return &PullSyncChangesInput{Cursor: token, PageSize: pageSize}, nil
}
