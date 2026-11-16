package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// sizeEntryCursorKeyPurpose 标记大小降序游标的独立密钥用途。
const sizeEntryCursorKeyPurpose = "cloudnest/size-entry-cursor/v1"

// sizeEntryCursorPayload 定义版本 1 大小和 ID 双降序游标。
type sizeEntryCursorPayload struct {
	// Version 标记载荷格式版本。
	Version uint8 `json:"version"`
	// UserID 绑定已认证用户，保存完整 uint64 十进制文本。
	UserID string `json:"user_id"`
	// Scope 绑定实际 all/starred 范围。
	Scope string `json:"scope"`
	// Sort 固定为大小和 ID 降序。
	Sort string `json:"sort"`
	// FilterHash 保存搜索及分类规范 JSON 的 SHA-256 十六进制摘要。
	FilterHash string `json:"filter_hash"`
	// FileSize 保存最后消费候选的完整无符号字节数，包括零字节。
	FileSize string `json:"file_size"`
	// EntryID 保存最后消费候选的完整无符号 ID。
	EntryID string `json:"entry_id"`
}

// SizeEntryCursorSigner 持有派生密钥，生成大小降序分页游标。
type SizeEntryCursorSigner struct {
	// signingKey 是构造时独立持有的 32 字节 HMAC 密钥。
	signingKey []byte
}

// NewSizeEntryCursorSigner 从至少 32 字节主密钥派生大小游标专用密钥。
func NewSizeEntryCursorSigner(masterKey []byte) (*SizeEntryCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("大小降序游标签名需要至少 32 字节主密钥")
	}
	// 2. 使用固定大小用途标签派生组件独立持有的密钥。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte(sizeEntryCursorKeyPurpose))
	return &SizeEntryCursorSigner{signingKey: derivation.Sum(nil)}, nil
}

// Sign 为 all/starred、size_desc 查询生成版本 1 大小降序游标。
// 搜索和分类按原文绑定，PageSize 按查询模型校验，允许在合法范围内调整。
func (signer *SizeEntryCursorSigner) Sign(userID uint64, query domain.GlobalEntryQuery, position domain.SizeEntryPosition) (string, error) {
	// 1. 检查签名器、用户、查询和大小位置。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("大小降序游标签名器尚未初始化")
	}
	if userID == 0 || !query.IsValid() || (query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) ||
		query.Sort != domain.GlobalEntrySortSizeDesc || !position.IsValid() {
		return "", domain.ErrInvalidSizeEntryCursor
	}

	// 2. 复用搜索及分类编码，计算筛选摘要。
	filters, err := json.Marshal(globalEntryCursorFilters{Search: query.Search, Kind: query.Kind})
	if err != nil {
		return "", fmt.Errorf("编码大小降序游标筛选条件失败: %w", err)
	}
	filterHash := sha256.Sum256(filters)

	// 3. 用完整十进制大小和 ID 组成版本化载荷。
	payload := sizeEntryCursorPayload{
		Version:    1,
		UserID:     strconv.FormatUint(userID, 10),
		Scope:      query.Scope,
		Sort:       query.Sort,
		FilterHash: hex.EncodeToString(filterHash[:]),
		FileSize:   strconv.FormatUint(position.FileSize, 10),
		EntryID:    strconv.FormatUint(position.ID, 10),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码大小降序分页游标失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 4. 计算大小用途 HMAC，检查长度并返回游标。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	cursor := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	if len(cursor) > domain.MaxSizeEntryCursorLength {
		return "", domain.ErrInvalidSizeEntryCursor
	}
	return cursor, nil
}
