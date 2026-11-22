package dto

import (
	"encoding/json"
	"errors"
)

// CreateSyncSnapshotRequest 保留 page_size 的 JSON 原值，区分省略和显式 null。
type CreateSyncSnapshotRequest struct {
	PageSize json.RawMessage `json:"page_size"`
}

// UnmarshalJSON 要求创建正文只含可选的 page_size，并拒绝重复键。
func (request *CreateSyncSnapshotRequest) UnmarshalJSON(body []byte) error {
	// 1. 读取唯一键对象，只允许精确拼写的 page_size。
	fields, valid := readUniqueJSONObject(body)
	pageSize, present := fields["page_size"]
	if !valid || len(fields) > 1 || (len(fields) == 1 && !present) {
		return errors.New("同步快照创建正文须为仅含可选 page_size 的 JSON 对象")
	}

	// 2. 保留原值，交给字段校验处理默认值及错误定位。
	request.PageSize = pageSize
	return nil
}

// ParseCreateSyncSnapshotPageSize 返回创建快照时固定的页大小。
func ParseCreateSyncSnapshotPageSize(request CreateSyncSnapshotRequest) (int, *FieldError) {
	// 1. 只有字段省略时使用默认页大小。
	if request.PageSize == nil {
		return 50, nil
	}

	// 2. 显式值须为 JSON 整数，且位于闭区间 1～100。
	var pageSize *int
	if err := json.Unmarshal(request.PageSize, &pageSize); err != nil || pageSize == nil || *pageSize < 1 || *pageSize > 100 {
		return 0, &FieldError{Field: "page_size", Message: "每页数量须为 1～100 的 JSON 整数"}
	}
	return *pageSize, nil
}
