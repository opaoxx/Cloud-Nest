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

// GlobalEntryCursorVerifier 核对版本 1 全局游标并还原内部续页位置。
type GlobalEntryCursorVerifier struct {
	// signer 复用专用密钥派生和规范载荷编码，归本组件持有。
	signer *GlobalEntryCursorSigner
}

// NewGlobalEntryCursorVerifier 复用已有签名器构造规则，持有独立派生密钥。
func NewGlobalEntryCursorVerifier(masterKey []byte) (*GlobalEntryCursorVerifier, error) {
	// 1. 复用主密钥校验及用途密钥派生。
	signer, err := NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &GlobalEntryCursorVerifier{signer: signer}, nil
}

// Verify 检查当前用户及查询绑定，成功返回独立的 UTC 微秒位置。
// 空文本和无效游标返回 ErrInvalidGlobalEntryCursor，组件未初始化单独报告。
func (verifier *GlobalEntryCursorVerifier) Verify(userID uint64, query domain.GlobalEntryQuery, cursor string) (*domain.FileEntryPosition, error) {
	// 1. 检查校验器、用户、查询和游标长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("全局游标校验器尚未初始化")
	}
	if userID == 0 || !query.IsValid() || query.Scope != domain.GlobalEntryScopeAll ||
		query.Sort != domain.GlobalEntrySortModifiedDesc || cursor == "" || len(cursor) > domain.MaxGlobalEntryCursorLength {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}

	// 2. 拆分规范 Base64URL 编码并核对 HMAC。
	encodedPayload, encodedMAC, found := strings.Cut(cursor, ".")
	if !found || encodedPayload == "" || encodedMAC == "" || strings.Contains(encodedMAC, ".") {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != encodedMAC {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != encodedPayload {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}

	// 3. 解析载荷，检查版本及当前用户和查询范围。
	var payload globalEntryCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) || payload.Scope != query.Scope || payload.Sort != query.Sort {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}

	// 4. 还原位置，复用签名器核对全部规范编码与筛选绑定。
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	modifiedAt, err := time.Parse(globalEntryCursorTimeLayout, payload.ModifiedAt)
	if err != nil {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	position := domain.FileEntryPosition{ModifiedAt: modifiedAt, ID: entryID}
	if !position.IsValid() {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	canonical, err := verifier.signer.Sign(userID, query, position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}
	return &position, nil
}
