package dto

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DecodePurgeEntryAcceptedData 校验并解码已保存的永久清理受理快照。
func DecodePurgeEntryAcceptedData(body []byte) (*PurgeEntryAcceptedData, error) {
	// 1. 读取恰好三个字符串字段，检查 JSON 完整性。
	if !utf8.Valid(body) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	fields, valid := readSnapshotStringFields(decoder, 3)
	if !valid {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 校验目标 ID、受理状态和对应的进度地址。
	entryID, status, progressURL := fields["entry_id"], fields["status"], fields["progress_url"]
	if _, valid := parsePathID(entryID); !valid || status != "PURGING" || progressURL != purgeEntryProgressURL(entryID) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 返回独立的受理快照。
	return &PurgeEntryAcceptedData{EntryID: entryID, Status: status, ProgressURL: progressURL}, nil
}
