package dto

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

var syncSnapshotTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// SyncSnapshotPageData 保存首页及续页的七个公开字段。
type SyncSnapshotPageData struct {
	SnapshotID        string            `json:"snapshot_id"`
	RootEntryID       string            `json:"root_entry_id"`
	HighWaterSequence string            `json:"high_water_sequence"`
	ExpiresAt         time.Time         `json:"expires_at"`
	Items             []json.RawMessage `json:"items"`
	// 未选用的凭据明确编码为 null。
	NextCursor    *string `json:"next_cursor"`
	BaselineToken *string `json:"baseline_token"`
}

// SyncSnapshotPageResponse 将快照公开页面与当次请求 ID 一起交付。
type SyncSnapshotPageResponse struct {
	Data      SyncSnapshotPageData `json:"data"`
	RequestID string               `json:"request_id"`
}

// NewSyncSnapshotPageData 将已签发的页面结果转换为 OpenAPI 约定的公开数据。
func NewSyncSnapshotPageData(result *domain.SyncSnapshotPageResult) (*SyncSnapshotPageData, error) {
	// 1. 核对页头、到期时间和互斥的凭据字段。
	if result == nil || result.SnapshotID.Version() != 4 || result.SnapshotID.Variant() != uuid.RFC4122 ||
		result.Header.RootEntryID == 0 || len(result.Items) > 100 || (result.NextCursor == nil) == (result.BaselineToken == nil) {
		return nil, domain.ErrInvalidFileEntryData
	}
	expiresAt := result.ExpiresAt.UTC()
	if expiresAt.Year() < 1000 || expiresAt.Year() > 9999 {
		return nil, domain.ErrInvalidFileEntryData
	}
	token := result.BaselineToken
	if result.NextCursor != nil {
		if len(result.Items) == 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		token = result.NextCursor
	}
	if !validSyncSnapshotTokenText(*token) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 复制已经编码的条目，空树保留非 nil 的空切片。
	items := make([]json.RawMessage, len(result.Items))
	for index, item := range result.Items {
		if len(item) < 2 || item[0] != '{' || !json.Valid(item) {
			return nil, domain.ErrInvalidFileEntryData
		}
		items[index] = bytes.Clone(item)
	}

	// 3. 将标识、序号和固定期限转换为公开字段，复制凭据文本。
	return &SyncSnapshotPageData{
		SnapshotID: result.SnapshotID.String(), RootEntryID: strconv.FormatUint(result.Header.RootEntryID, 10),
		HighWaterSequence: strconv.FormatUint(result.Header.HighWaterSequence, 10), ExpiresAt: expiresAt,
		Items: items, NextCursor: copyEntryText(result.NextCursor), BaselineToken: copyEntryText(result.BaselineToken),
	}, nil
}

func validSyncSnapshotTokenText(token string) bool {
	return len(token) <= domain.MaxSyncSnapshotTokenLength && syncSnapshotTokenPattern.MatchString(token)
}
