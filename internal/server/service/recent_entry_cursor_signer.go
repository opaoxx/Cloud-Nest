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
	// recentEntryCursorKeyPurpose 标记最近使用游标的独立密钥用途。
	recentEntryCursorKeyPurpose = "cloudnest/recent-entry-cursor/v1"
	// recentEntryCursorTimeLayout 固定 UTC 六位小数，保留数据库访问时间精度。
	recentEntryCursorTimeLayout = "2006-01-02T15:04:05.000000Z"
)

// recentEntryCursorPayload 定义版本 1 最近使用访问时间排序游标。
type recentEntryCursorPayload struct {
	// Version 标记载荷格式版本。
	Version uint8 `json:"version"`
	// UserID 绑定已认证用户，保存完整 uint64 十进制文本。
	UserID string `json:"user_id"`
	// Scope 固定为当前最近使用范围。
	Scope string `json:"scope"`
	// Sort 固定为最近访问时间和 ID 降序。
	Sort string `json:"sort"`
	// FilterHash 保存搜索及分类规范 JSON 的 SHA-256 十六进制摘要。
	FilterHash string `json:"filter_hash"`
	// LastOpenedAt 保存最后消费候选的 UTC 微秒访问时间。
	LastOpenedAt string `json:"last_opened_at"`
	// EntryID 保存最后消费候选的完整无符号 ID。
	EntryID string `json:"entry_id"`
}

// RecentEntryCursorSigner 持有派生密钥，生成最近使用分页游标。
type RecentEntryCursorSigner struct {
	// signingKey 是构造时独立持有的 32 字节 HMAC 密钥。
	signingKey []byte
}

// NewRecentEntryCursorSigner 从至少 32 字节主密钥派生最近使用游标专用密钥。
func NewRecentEntryCursorSigner(masterKey []byte) (*RecentEntryCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("最近使用游标签名需要至少 32 字节主密钥")
	}
	// 2. 使用固定用途标签派生组件独立持有的密钥。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte(recentEntryCursorKeyPurpose))
	return &RecentEntryCursorSigner{signingKey: derivation.Sum(nil)}, nil
}

// Sign 为 recent/last_opened_desc 查询生成版本 1 最近使用游标。
// 搜索和分类按原文绑定，PageSize 按查询模型校验，允许在合法范围内调整。
func (signer *RecentEntryCursorSigner) Sign(userID uint64, query domain.GlobalEntryQuery, position domain.RecentEntryPosition) (string, error) {
	// 1. 检查签名器、用户、最近使用查询与访问位置。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("最近使用游标签名器尚未初始化")
	}
	if userID == 0 || !query.IsValid() || query.Scope != domain.GlobalEntryScopeRecent ||
		query.Sort != domain.GlobalEntrySortLastOpenedDesc || !position.IsValid() {
		return "", domain.ErrInvalidRecentEntryCursor
	}

	// 2. 复用搜索和分类编码，生成筛选摘要。
	filters, err := json.Marshal(globalEntryCursorFilters{Search: query.Search, Kind: query.Kind})
	if err != nil {
		return "", fmt.Errorf("编码最近使用游标筛选条件失败: %w", err)
	}
	filterHash := sha256.Sum256(filters)

	// 3. 将访问位置规范为 UTC 微秒时间并编码载荷。
	payload := recentEntryCursorPayload{
		Version:      1,
		UserID:       strconv.FormatUint(userID, 10),
		Scope:        query.Scope,
		Sort:         query.Sort,
		FilterHash:   hex.EncodeToString(filterHash[:]),
		LastOpenedAt: position.LastOpenedAt.UTC().Format(recentEntryCursorTimeLayout),
		EntryID:      strconv.FormatUint(position.ID, 10),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码最近使用分页游标失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 4. 计算 HMAC 签名，返回 URL 安全游标。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	cursor := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	if len(cursor) > domain.MaxRecentEntryCursorLength {
		return "", domain.ErrInvalidRecentEntryCursor
	}
	return cursor, nil
}
