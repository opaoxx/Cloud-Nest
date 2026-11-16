package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// NameEntryCursorVerifier 核对版本 1 名称游标并还原内部续页位置。
type NameEntryCursorVerifier struct {
	// signer 复用专用密钥派生和规范载荷编码，归本组件持有。
	signer *NameEntryCursorSigner
}

// NewNameEntryCursorVerifier 复用已有签名器构造规则，持有独立派生密钥。
func NewNameEntryCursorVerifier(masterKey []byte) (*NameEntryCursorVerifier, error) {
	// 1. 复用主密钥校验及用途密钥派生。
	signer, err := NewNameEntryCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &NameEntryCursorVerifier{signer: signer}, nil
}

// Verify 检查当前用户及 all/starred 查询绑定，成功返回独立的名称键/ID 位置。
// 空文本和无效游标返回 ErrInvalidNameEntryCursor，组件未初始化单独报告。
func (verifier *NameEntryCursorVerifier) Verify(userID uint64, query domain.GlobalEntryQuery, cursor string) (*domain.NameEntryPosition, error) {
	// 1. 检查组件、用户、查询条件和游标长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("名称游标校验器尚未初始化")
	}
	if userID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) ||
		query.Sort != domain.GlobalEntrySortNameAsc || cursor == "" || len(cursor) > domain.MaxNameEntryCursorLength {
		return nil, domain.ErrInvalidNameEntryCursor
	}

	// 2. 拆分文本，核对规范 Base64URL 编码及 HMAC。
	encodedPayload, encodedMAC, found := strings.Cut(cursor, ".")
	if !found || encodedPayload == "" || encodedMAC == "" || strings.Contains(encodedMAC, ".") {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != encodedMAC {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != encodedPayload {
		return nil, domain.ErrInvalidNameEntryCursor
	}

	// 3. 解析载荷，检查版本、用户、范围和排序。
	var payload nameEntryCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) || payload.Scope != query.Scope || payload.Sort != query.Sort {
		return nil, domain.ErrInvalidNameEntryCursor
	}

	// 4. 还原名称键和 ID，复用签名器核对规范编码与筛选绑定。
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	nameKey, err := base64.RawURLEncoding.Strict().DecodeString(payload.NameKey)
	if err != nil || base64.RawURLEncoding.EncodeToString(nameKey) != payload.NameKey {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	position := domain.NameEntryPosition{NameKey: nameKey, ID: entryID}
	if !position.IsValid() {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	canonical, err := verifier.signer.Sign(userID, query, position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidNameEntryCursor
	}
	return &position, nil
}
