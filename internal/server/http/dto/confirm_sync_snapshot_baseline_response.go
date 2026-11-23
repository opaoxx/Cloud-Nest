package dto

import (
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ConfirmSyncSnapshotBaselineData 保存已提交的设备确认结果。
type ConfirmSyncSnapshotBaselineData struct {
	Status      string `json:"status"`
	AckSequence string `json:"ack_sequence"`
}

// ConfirmSyncSnapshotBaselineResponse 将基线确认结果与当次请求 ID 关联。
type ConfirmSyncSnapshotBaselineResponse struct {
	Data      ConfirmSyncSnapshotBaselineData `json:"data"`
	RequestID string                          `json:"request_id"`
}

// NewConfirmSyncSnapshotBaselineData 将已提交的确认位置转换为公开数据。
func NewConfirmSyncSnapshotBaselineData(ackSequence uint64) ConfirmSyncSnapshotBaselineData {
	// 1. 将确认序号转为十进制字符串，保留零值和完整精度。
	sequence := strconv.FormatUint(ackSequence, 10)

	// 2. 组装 ACTIVE 成功数据，供响应外层附上 request_id。
	return ConfirmSyncSnapshotBaselineData{
		Status:      string(domain.SyncDeviceStatusActive),
		AckSequence: sequence,
	}
}
