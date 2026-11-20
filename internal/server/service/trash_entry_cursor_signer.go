package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	// 版本 1 的用途标签绑定回收时间和目录项 ID 双降序分页。
	trashEntryCursorKeyPurpose = "cloudnest/trash-entry-cursor/v1"
	trashEntryCursorTimeLayout = "2006-01-02T15:04:05.000000Z"
)

// trashEntryCursorPayload 保存版本、用户和最后消费的候选位置。
type trashEntryCursorPayload struct {
	Version   uint8  `json:"version"`
	UserID    string `json:"user_id"`
	DeletedAt string `json:"deleted_at"`
	EntryID   string `json:"entry_id"`
}

// TrashEntryCursorSigner 持有回收站游标专用签名密钥。
type TrashEntryCursorSigner struct {
	signingKey []byte
}

// NewTrashEntryCursorSigner 派生回收站游标专用密钥。
func NewTrashEntryCursorSigner(masterKey []byte) (*TrashEntryCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("回收站游标签名需要至少 32 字节主密钥")
	}

	// 2. 使用回收站用途标签派生签名密钥。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte(trashEntryCursorKeyPurpose))
	return &TrashEntryCursorSigner{signingKey: derivation.Sum(nil)}, nil
}

// Sign 为回收站续读位置签发游标。
// 用户 ID 来自已认证身份，位置可以指向扫描预算用完时的隐藏候选。
func (signer *TrashEntryCursorSigner) Sign(userID uint64, position domain.TrashEntryPosition) (string, error) {
	// 1. 核对签名密钥、用户 ID 和续读位置。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("回收站游标签名器尚未初始化")
	}
	if userID == 0 || !position.IsValid() {
		return "", domain.ErrInvalidTrashEntryCursor
	}

	// 2. 将用户 ID、回收时间和目录项 ID 编码为版本 1 载荷。
	payload := trashEntryCursorPayload{
		Version:   1,
		UserID:    strconv.FormatUint(userID, 10),
		DeletedAt: position.DeletedAt.UTC().Format(trashEntryCursorTimeLayout),
		EntryID:   strconv.FormatUint(position.ID, 10),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码回收站分页游标失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 3. 对载荷签名，返回符合长度限制的 Base64URL 游标。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	cursor := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	if len(cursor) > domain.MaxTrashEntryCursorLength {
		return "", domain.ErrInvalidTrashEntryCursor
	}
	return cursor, nil
}
