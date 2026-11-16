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
	// globalEntryCursorKeyPurpose 标记全局游标的独立密钥用途。
	globalEntryCursorKeyPurpose = "cloudnest/global-entry-cursor/v1"
	// globalEntryCursorTimeLayout 固定 UTC 六位小数，保留数据库微秒位置。
	globalEntryCursorTimeLayout = "2006-01-02T15:04:05.000000Z"
)

// globalEntryCursorFilters 固定筛选摘要的字段顺序和原文值。
type globalEntryCursorFilters struct {
	// Search 绑定当前原始搜索词，包括空白和特殊字符。
	Search string `json:"q"`
	// Kind 绑定当前小写分类，空值表示全部类型。
	Kind string `json:"kind"`
}

// globalEntryCursorPayload 定义版本 1 的全局修改时间排序游标。
type globalEntryCursorPayload struct {
	// Version 标记载荷格式版本。
	Version uint8 `json:"version"`
	// UserID 绑定已认证用户，保存完整 uint64 十进制文本。
	UserID string `json:"user_id"`
	// Scope 绑定当前全局候选范围。
	Scope string `json:"scope"`
	// Sort 绑定当前实际执行的排序。
	Sort string `json:"sort"`
	// FilterHash 保存搜索及分类规范 JSON 的 SHA-256 十六进制摘要。
	FilterHash string `json:"filter_hash"`
	// ModifiedAt 保存最后消费候选的 UTC 微秒修改时间。
	ModifiedAt string `json:"modified_at"`
	// EntryID 保存最后消费候选的完整无符号 ID。
	EntryID string `json:"entry_id"`
}

// GlobalEntryCursorSigner 使用本组件持有的派生密钥签发全局分页游标。
type GlobalEntryCursorSigner struct {
	// signingKey 是构造时生成并独立持有的 32 字节 HMAC 密钥。
	signingKey []byte
}

// NewGlobalEntryCursorSigner 从至少 32 字节主密钥派生全局游标专用密钥。
func NewGlobalEntryCursorSigner(masterKey []byte) (*GlobalEntryCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("全局游标签名需要至少 32 字节主密钥")
	}
	// 2. 使用固定用途标签派生组件独立持有的密钥。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte(globalEntryCursorKeyPurpose))
	return &GlobalEntryCursorSigner{signingKey: derivation.Sum(nil)}, nil
}

// Sign 为 all/modified_desc 查询的最后消费位置生成版本 1 游标。
// 搜索与分类按原文绑定，每页数量按模型校验，合法数量调整沿用同一查询绑定。
func (signer *GlobalEntryCursorSigner) Sign(userID uint64, query domain.GlobalEntryQuery, position domain.FileEntryPosition) (string, error) {
	// 1. 检查签名器、用户、查询与续页位置。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("全局游标签名器尚未初始化")
	}
	if userID == 0 || !query.IsValid() || query.Scope != domain.GlobalEntryScopeAll ||
		query.Sort != domain.GlobalEntrySortModifiedDesc || !position.IsValid() {
		return "", domain.ErrInvalidGlobalEntryCursor
	}

	// 2. 编码搜索和分类条件，生成筛选摘要。
	filters, err := json.Marshal(globalEntryCursorFilters{Search: query.Search, Kind: query.Kind})
	if err != nil {
		return "", fmt.Errorf("编码全局游标筛选条件失败: %w", err)
	}
	filterHash := sha256.Sum256(filters)

	// 3. 规范化位置并编码游标载荷。
	payload := globalEntryCursorPayload{
		Version:    1,
		UserID:     strconv.FormatUint(userID, 10),
		Scope:      query.Scope,
		Sort:       query.Sort,
		FilterHash: hex.EncodeToString(filterHash[:]),
		ModifiedAt: position.ModifiedAt.UTC().Format(globalEntryCursorTimeLayout),
		EntryID:    strconv.FormatUint(position.ID, 10),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码全局分页游标失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 4. 计算 HMAC 签名，返回 URL 安全游标。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	cursor := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	if len(cursor) > domain.MaxGlobalEntryCursorLength {
		return "", domain.ErrInvalidGlobalEntryCursor
	}
	return cursor, nil
}
