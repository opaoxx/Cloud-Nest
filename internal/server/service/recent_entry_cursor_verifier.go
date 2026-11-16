package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecentEntryCursorVerifier 校验版本 1 最近使用游标并还原访问位置。
type RecentEntryCursorVerifier struct {
	// signer 复用最近使用密钥派生和规范载荷编码，归本组件持有。
	signer *RecentEntryCursorSigner
}

// NewRecentEntryCursorVerifier 复用最近使用签名器规则，持有独立派生密钥。
func NewRecentEntryCursorVerifier(masterKey []byte) (*RecentEntryCursorVerifier, error) {
	// 1. 复用主密钥校验和最近使用用途密钥派生。
	signer, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &RecentEntryCursorVerifier{signer: signer}, nil
}

// Verify 检查当前用户和 recent 查询绑定，成功返回独立的 UTC 微秒访问位置。
// 空文本和无效游标返回 ErrInvalidRecentEntryCursor，组件未初始化单独报告。
func (verifier *RecentEntryCursorVerifier) Verify(userID uint64, query domain.GlobalEntryQuery, cursor string) (*domain.RecentEntryPosition, error) {
	// 1. 检查校验器、用户、最近使用查询和游标长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("最近使用游标校验器尚未初始化")
	}
	if userID == 0 || !query.IsValid() || query.Scope != domain.GlobalEntryScopeRecent ||
		query.Sort != domain.GlobalEntrySortLastOpenedDesc || cursor == "" || len(cursor) > domain.MaxRecentEntryCursorLength {
		return nil, domain.ErrInvalidRecentEntryCursor
	}

	// 2. 拆分规范 Base64URL 编码，先核对 HMAC。
	encodedPayload, encodedMAC, found := strings.Cut(cursor, ".")
	if !found || encodedPayload == "" || encodedMAC == "" || strings.Contains(encodedMAC, ".") {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != encodedMAC {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != encodedPayload {
		return nil, domain.ErrInvalidRecentEntryCursor
	}

	// 3. 解析载荷，检查版本及当前用户、范围和排序。
	var payload recentEntryCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) || payload.Scope != query.Scope || payload.Sort != query.Sort {
		return nil, domain.ErrInvalidRecentEntryCursor
	}

	// 4. 还原访问位置，复用签名器核对规范编码和筛选绑定。
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	openedAt, err := time.Parse(recentEntryCursorTimeLayout, payload.LastOpenedAt)
	if err != nil {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	position := domain.RecentEntryPosition{LastOpenedAt: openedAt, ID: entryID}
	if !position.IsValid() {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	canonical, err := verifier.signer.Sign(userID, query, position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidRecentEntryCursor
	}
	return &position, nil
}
