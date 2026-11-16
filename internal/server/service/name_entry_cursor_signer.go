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

const (
	// nameEntryCursorKeyPurpose 标记名称升序游标的独立密钥用途。
	nameEntryCursorKeyPurpose = "cloudnest/name-entry-cursor/v1"
)

// nameEntryCursorPayload 定义版本 1 名称键升序游标。
type nameEntryCursorPayload struct {
	// Version 标记载荷格式版本。
	Version uint8 `json:"version"`
	// UserID 绑定已认证用户，保存完整 uint64 十进制文本。
	UserID string `json:"user_id"`
	// Scope 绑定实际 all/starred 范围。
	Scope string `json:"scope"`
	// Sort 固定为名称键和 ID 升序。
	Sort string `json:"sort"`
	// FilterHash 保存搜索及分类规范 JSON 的 SHA-256 十六进制摘要。
	FilterHash string `json:"filter_hash"`
	// NameKey 用无填充 Base64URL 保存最后消费候选的原始名称键。
	NameKey string `json:"name_key"`
	// EntryID 保存最后消费候选的完整无符号 ID。
	EntryID string `json:"entry_id"`
}

// NameEntryCursorSigner 持有派生密钥，生成名称升序分页游标。
type NameEntryCursorSigner struct {
	// signingKey 是构造时独立持有的 32 字节 HMAC 密钥。
	signingKey []byte
}

// NewNameEntryCursorSigner 从至少 32 字节主密钥派生名称升序游标专用密钥。
func NewNameEntryCursorSigner(masterKey []byte) (*NameEntryCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("名称升序游标签名需要至少 32 字节主密钥")
	}
	// 2. 使用固定用途标签派生组件独立持有的密钥。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte(nameEntryCursorKeyPurpose))
	return &NameEntryCursorSigner{signingKey: derivation.Sum(nil)}, nil
}

// Sign 为 all/starred、name_asc 查询生成版本 1 名称升序游标。
// 搜索和分类按原文绑定，PageSize 按查询模型校验，允许在合法范围内调整。
func (signer *NameEntryCursorSigner) Sign(userID uint64, query domain.GlobalEntryQuery, position domain.NameEntryPosition) (string, error) {
	// 1. 检查签名器、用户、查询条件和名称位置。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("名称升序游标签名器尚未初始化")
	}
	if userID == 0 || !query.IsValid() || (query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) ||
		query.Sort != domain.GlobalEntrySortNameAsc || !position.IsValid() {
		return "", domain.ErrInvalidNameEntryCursor
	}

	// 2. 复用搜索和分类编码，生成筛选摘要。
	filters, err := json.Marshal(globalEntryCursorFilters{Search: query.Search, Kind: query.Kind})
	if err != nil {
		return "", fmt.Errorf("编码名称升序游标筛选条件失败: %w", err)
	}
	filterHash := sha256.Sum256(filters)

	// 3. 编码原始名称键和完整 ID，组成版本化载荷。
	payload := nameEntryCursorPayload{
		Version:    1,
		UserID:     strconv.FormatUint(userID, 10),
		Scope:      query.Scope,
		Sort:       query.Sort,
		FilterHash: hex.EncodeToString(filterHash[:]),
		NameKey:    base64.RawURLEncoding.EncodeToString(position.NameKey),
		EntryID:    strconv.FormatUint(position.ID, 10),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码名称升序分页游标失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 4. 计算 HMAC，检查长度并返回 URL 安全文本。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	cursor := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	if len(cursor) > domain.MaxNameEntryCursorLength {
		return "", domain.ErrInvalidNameEntryCursor
	}
	return cursor, nil
}
