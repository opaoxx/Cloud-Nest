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

// TrashEntryCursorVerifier 使用回收站专用密钥验证版本 1 游标。
type TrashEntryCursorVerifier struct {
	// signer 提供相同的用途密钥和规范编码。
	signer *TrashEntryCursorSigner
}

// NewTrashEntryCursorVerifier 复用签发器的主密钥校验与用途密钥派生。
func NewTrashEntryCursorVerifier(masterKey []byte) (*TrashEntryCursorVerifier, error) {
	// 1. 派生回收站游标专用密钥。
	signer, err := NewTrashEntryCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &TrashEntryCursorVerifier{signer: signer}, nil
}

// Verify 校验回收站续页游标并还原位置。
// userID 来自已认证身份。空文本或无效游标返回 ErrInvalidTrashEntryCursor。
func (verifier *TrashEntryCursorVerifier) Verify(userID uint64, cursor string) (*domain.TrashEntryPosition, error) {
	// 1. 检查校验器、当前用户和游标长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("回收站游标校验器尚未初始化")
	}
	if userID == 0 || cursor == "" || len(cursor) > domain.MaxTrashEntryCursorLength {
		return nil, domain.ErrInvalidTrashEntryCursor
	}

	// 2. 拆分游标并验证 HMAC，再解码载荷。
	encodedPayload, encodedMAC, found := strings.Cut(cursor, ".")
	if !found || encodedPayload == "" || len(encodedMAC) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil {
		return nil, domain.ErrInvalidTrashEntryCursor
	}

	// 3. 核对版本和用户，还原回收时间与目录项 ID。
	var payload trashEntryCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	deletedAt, err := time.Parse(trashEntryCursorTimeLayout, payload.DeletedAt)
	if err != nil {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	position := domain.TrashEntryPosition{ID: entryID, DeletedAt: deletedAt}

	// 4. 复用签发器核对规范编码，返回续读位置。
	canonical, err := verifier.signer.Sign(userID, position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidTrashEntryCursor
	}
	return &position, nil
}
