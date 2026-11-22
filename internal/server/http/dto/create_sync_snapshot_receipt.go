package dto

import (
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const createSyncSnapshotReceiptVersion = 1

type createSyncSnapshotReceiptBody struct {
	Version    uint8  `json:"version"`
	SnapshotID string `json:"snapshot_id"`
	DeviceID   string `json:"device_id"`
	Generation string `json:"generation"`
}

// EncodeCreateSyncSnapshotReceipt 编码幂等记录的快照定位元数据。
func EncodeCreateSyncSnapshotReceipt(locator domain.SyncSnapshotReceiptLocator) (json.RawMessage, error) {
	// 1. 核对快照 UUID、设备 ID 和代次的范围。
	if !validSyncSnapshotReceiptLocator(locator) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 按版本化 JSON 结构保留设备 ID 精度及代次的全部字节。
	return json.Marshal(createSyncSnapshotReceiptBody{
		Version:    createSyncSnapshotReceiptVersion,
		SnapshotID: locator.SnapshotID.String(),
		DeviceID:   strconv.FormatUint(locator.DeviceID, 10),
		Generation: hex.EncodeToString(locator.Generation[:]),
	})
}

// DecodeCreateSyncSnapshotReceipt 恢复定位数据，支持数据库改变 JSON 字段顺序和空白。
func DecodeCreateSyncSnapshotReceipt(body json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error) {
	// 1. 核对对象只含当前版本的四个字段，且各键只出现一次。
	fields, valid := readUniqueJSONObject(body)
	var payload createSyncSnapshotReceiptBody
	if !valid || len(fields) != 4 ||
		json.Unmarshal(fields["version"], &payload.Version) != nil || payload.Version != createSyncSnapshotReceiptVersion ||
		json.Unmarshal(fields["snapshot_id"], &payload.SnapshotID) != nil ||
		json.Unmarshal(fields["device_id"], &payload.DeviceID) != nil ||
		json.Unmarshal(fields["generation"], &payload.Generation) != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 还原规范小写 UUID v4 和非零的完整十进制设备 ID。
	snapshotID, snapshotError := uuid.Parse(payload.SnapshotID)
	deviceID, validDevice := parsePathID(payload.DeviceID)
	if snapshotError != nil || snapshotID.String() != payload.SnapshotID || !validDevice {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 还原完整的非零代次，核对小写十六进制表示。
	generation, generationError := hex.DecodeString(payload.Generation)
	if generationError != nil || len(generation) != len(domain.SyncCursorGeneration{}) ||
		hex.EncodeToString(generation) != payload.Generation {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	locator := domain.SyncSnapshotReceiptLocator{
		SnapshotID: snapshotID, DeviceID: deviceID, Generation: domain.SyncCursorGeneration(generation),
	}
	if !validSyncSnapshotReceiptLocator(locator) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return &locator, nil
}

func validSyncSnapshotReceiptLocator(locator domain.SyncSnapshotReceiptLocator) bool {
	return locator.SnapshotID.Version() == 4 && locator.SnapshotID.Variant() == uuid.RFC4122 &&
		locator.DeviceID != 0 && locator.Generation != (domain.SyncCursorGeneration{})
}
