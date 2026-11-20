package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildPurgeEntryRequestHash 为确认永久清理的参数生成版本化请求摘要。
// 失败时返回零值摘要，凭证验签和到期检查由首次受理流程执行。
func BuildPurgeEntryRequestHash(entryID, expectedRevision uint64, previewDigest string) ([sha256.Size]byte, error) {
	// 1. 校验目标 ID、预期版本和凭证格式。
	if entryID == 0 || expectedRevision == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成永久清理确认请求摘要需要非零目标 ID 和预期版本")
	}
	if !domain.ValidPurgePreviewDigestFormat(previewDigest) {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}

	// 2. 按固定字段顺序编码操作名、目标、版本和凭证原文。
	request := struct {
		Operation        string `json:"operation"`
		EntryID          string `json:"entry_id"`
		ExpectedRevision string `json:"expected_revision"`
		PreviewDigest    string `json:"preview_digest"`
	}{
		Operation:        domain.PurgeEntryOperation,
		EntryID:          strconv.FormatUint(entryID, 10),
		ExpectedRevision: strconv.FormatUint(expectedRevision, 10),
		PreviewDigest:    previewDigest,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码永久清理确认请求摘要失败: %w", err)
	}

	// 3. 计算并返回 SHA-256 摘要。
	return sha256.Sum256(encoded), nil
}
