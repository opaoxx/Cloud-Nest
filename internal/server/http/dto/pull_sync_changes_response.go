package dto

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangeEventData 保存单条公开的增量变更。
type SyncChangeEventData struct {
	Sequence  string          `json:"sequence"`
	EntryID   string          `json:"entry_id"`
	Revision  string          `json:"revision"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

// PullSyncChangesData 保存已交付的事件和后续读取所需的公开信息。
type PullSyncChangesData struct {
	Events            []SyncChangeEventData `json:"events"`
	HighWaterSequence string                `json:"high_water_sequence"`
	NextCursor        string                `json:"next_cursor"`
	HasMore           bool                  `json:"has_more"`
}

// PullSyncChangesResponse 将成功数据与当次请求 ID 一起交付。
type PullSyncChangesResponse struct {
	Data      PullSyncChangesData `json:"data"`
	RequestID string              `json:"request_id"`
}

// NewPullSyncChangesData 将已核验并签发的页面转换为公开数据。
func NewPullSyncChangesData(result *domain.SyncChangePullResult) (*PullSyncChangesData, error) {
	// 1. 核对页大小、游标格式与可交付的事件字段。
	if result == nil || len(result.Events) > 100 || !validSyncSnapshotTokenText(result.NextCursor) {
		return nil, domain.ErrInvalidSyncChangePage
	}
	events := make([]SyncChangeEventData, len(result.Events))
	var lastSequence uint64
	for index, event := range result.Events {
		payload := bytes.TrimLeft(event.Payload, " \t\r\n")
		if event.Sequence == 0 || event.Sequence > result.HighWaterSequence ||
			(index > 0 && event.Sequence-1 != lastSequence) ||
			event.EntryID == 0 || event.Revision == 0 || event.EventType == "" ||
			len(payload) == 0 || payload[0] != '{' || !utf8.Valid(payload) || !json.Valid(payload) {
			return nil, domain.ErrInvalidSyncChangePage
		}

		// 2. 把序号和标识转为十进制字符串，复制原始载荷。
		events[index] = SyncChangeEventData{
			Sequence:  strconv.FormatUint(event.Sequence, 10),
			EntryID:   strconv.FormatUint(event.EntryID, 10),
			Revision:  strconv.FormatUint(event.Revision, 10),
			EventType: event.EventType,
			Payload:   bytes.Clone(event.Payload),
		}
		lastSequence = event.Sequence
	}

	// 3. 核对后续页标记并组装公开数据。
	if result.HasMore != (lastSequence != 0 && lastSequence < result.HighWaterSequence) {
		return nil, domain.ErrInvalidSyncChangePage
	}
	return &PullSyncChangesData{
		Events:            events,
		HighWaterSequence: strconv.FormatUint(result.HighWaterSequence, 10),
		NextCursor:        result.NextCursor,
		HasMore:           result.HasMore,
	}, nil
}
