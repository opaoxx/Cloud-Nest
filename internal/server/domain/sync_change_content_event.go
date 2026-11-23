package domain

import (
	"encoding/hex"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// ValidateSyncChangeContentEvent 核对文件发布或内容替换事件的六字段载荷。
func ValidateSyncChangeContentEvent(event SyncChangeEvent) error {
	// 1. 确认事件类型，并严格解码六字段载荷。
	if event.EventType != FilePublishedEventType && event.EventType != FileContentReplacedEventType {
		return fmt.Errorf("%w: 六字段载荷的事件类型无效", ErrInvalidSyncChangePage)
	}
	var payload struct {
		entryChangePayload
		SizeBytes *uint64 `json:"size_bytes"`
		SHA256    string  `json:"sha256"`
	}
	if err := jsonv2.Unmarshal(event.Payload, &payload, jsonv2.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%w: 六字段事件载荷格式无效", ErrInvalidSyncChangePage)
	}

	// 2. 复用条目 ID、父目录、名称和版本的校验。
	if err := validateSyncChangeEntryPayload(event, payload.entryChangePayload); err != nil {
		return err
	}

	// 3. 检查字节数范围和 SHA-256 的小写十六进制格式。
	const maxJSONSafeInteger = 1<<53 - 1
	if payload.SizeBytes == nil || *payload.SizeBytes > maxJSONSafeInteger {
		return fmt.Errorf("%w: 文件字节数须为 0～%d 的 JSON 整数", ErrInvalidSyncChangePage, maxJSONSafeInteger)
	}
	digest, valid := DecodeUploadPartSHA256(payload.SHA256)
	if !valid || hex.EncodeToString(digest[:]) != payload.SHA256 {
		return fmt.Errorf("%w: SHA-256 须为 64 位小写十六进制文本", ErrInvalidSyncChangePage)
	}
	return nil
}
