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

// SizeEntryCursorVerifier 核对版本 1 大小游标并还原内部续页位置。
type SizeEntryCursorVerifier struct {
	// signer 复用专用密钥派生和规范载荷编码，归本组件持有。
	signer *SizeEntryCursorSigner
}

// NewSizeEntryCursorVerifier 复用已有签名器构造规则，持有独立派生密钥。
func NewSizeEntryCursorVerifier(masterKey []byte) (*SizeEntryCursorVerifier, error) {
	// 1. 复用主密钥校验及用途密钥派生。
	signer, err := NewSizeEntryCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &SizeEntryCursorVerifier{signer: signer}, nil
}

// Verify 检查当前用户及 all/starred 查询绑定，成功返回独立的大小/ID 位置。
// 空文本和无效游标返回 ErrInvalidSizeEntryCursor，组件未初始化单独报告。
func (verifier *SizeEntryCursorVerifier) Verify(userID uint64, query domain.GlobalEntryQuery, cursor string) (*domain.SizeEntryPosition, error) {
	// 1. 检查校验器、用户、查询条件和游标长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("大小游标校验器尚未初始化")
	}
	if userID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) ||
		query.Sort != domain.GlobalEntrySortSizeDesc || cursor == "" || len(cursor) > domain.MaxSizeEntryCursorLength {
		return nil, domain.ErrInvalidSizeEntryCursor
	}

	// 2. 拆分文本，先核对规范签名编码及大小用途 HMAC。
	encodedPayload, encodedMAC, found := strings.Cut(cursor, ".")
	if !found || encodedPayload == "" || encodedMAC == "" || strings.Contains(encodedMAC, ".") {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != encodedMAC {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != encodedPayload {
		return nil, domain.ErrInvalidSizeEntryCursor
	}

	// 3. 解析载荷，检查版本、用户、范围和排序。
	var payload sizeEntryCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) || payload.Scope != query.Scope || payload.Sort != query.Sort {
		return nil, domain.ErrInvalidSizeEntryCursor
	}

	// 4. 还原大小和 ID，复用签名器核对筛选绑定及规范编码。
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	fileSize, err := strconv.ParseUint(payload.FileSize, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	position := domain.SizeEntryPosition{FileSize: fileSize, ID: entryID}
	if !position.IsValid() {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	canonical, err := verifier.signer.Sign(userID, query, position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidSizeEntryCursor
	}
	return &position, nil
}
