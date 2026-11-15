package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DirectoryCursorVerifier 核对版本 1 普通目录默认排序游标。
type DirectoryCursorVerifier struct {
	// signer 复用专用密钥派生及规范载荷编码，归校验器持有。
	signer *DirectoryCursorSigner
}

// NewDirectoryCursorVerifier 复用签名器构造规则，保存本组件的派生密钥。
func NewDirectoryCursorVerifier(masterKey []byte) (*DirectoryCursorVerifier, error) {
	// 1. 检查主密钥并沿用已有的用途密钥派生。
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &DirectoryCursorVerifier{signer: signer}, nil
}

// Verify 检查当前已认证用户和父目录绑定，成功返回独立的分页位置。
// 空文本及无效输入返回 ErrInvalidDirectoryCursor，初始化错误单独报告。
func (verifier *DirectoryCursorVerifier) Verify(userID, parentID uint64, cursor string) (*domain.FileEntryPosition, error) {
	// 1. 检查校验器、当前范围和输入长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("目录游标校验器尚未初始化")
	}
	if userID == 0 || parentID == 0 || cursor == "" || len(cursor) > domain.MaxDirectoryCursorLength {
		return nil, domain.ErrInvalidDirectoryCursor
	}

	// 2. 拆分规范 Base64URL 编码并核对 HMAC。
	encodedPayload, encodedMAC, found := strings.Cut(cursor, ".")
	if !found || encodedPayload == "" || encodedMAC == "" || strings.Contains(encodedMAC, ".") {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != encodedMAC {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != encodedPayload {
		return nil, domain.ErrInvalidDirectoryCursor
	}

	// 3. 解析载荷，核对版本、用户、目录及查询条件。
	var payload directoryCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	filterHash := sha256.Sum256([]byte(directoryCursorEmptyFilters))
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) ||
		payload.ParentID != strconv.FormatUint(parentID, 10) || payload.Scope != "directory" ||
		payload.Sort != "modified_desc" || payload.FilterHash != hex.EncodeToString(filterHash[:]) {
		return nil, domain.ErrInvalidDirectoryCursor
	}

	// 4. 还原并检查分页位置，核对规范编码后返回。
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	modifiedAt, err := time.Parse(directoryCursorTimeLayout, payload.ModifiedAt)
	if err != nil {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	position := domain.FileEntryPosition{ModifiedAt: modifiedAt, ID: entryID}
	if !position.IsValid() {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	canonical, err := verifier.signer.Sign(userID, parentID, position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidDirectoryCursor
	}
	return &position, nil
}
