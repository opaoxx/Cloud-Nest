package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	syncSnapshotCursorKeyPurpose = "cloudnest/sync-snapshot-cursor/v1"
	syncSnapshotCursorVersion    = 1
)

// syncSnapshotCursorPayload 固定第 1 版快照续页凭据的字段和编码顺序。
type syncSnapshotCursorPayload struct {
	Version      uint8  `json:"version"`
	SnapshotID   string `json:"snapshot_id"`
	UserID       string `json:"user_id"`
	DeviceID     string `json:"device_id"`
	Generation   string `json:"generation"`
	PageSize     int    `json:"page_size"`
	NextPosition int    `json:"next_position"`
	ExpiresAt    string `json:"expires_at"`
}

// SyncSnapshotCursorSigner 持有快照续页凭据的专用 HMAC 密钥。
type SyncSnapshotCursorSigner struct {
	signingKey []byte
}

// NewSyncSnapshotCursorSigner 从现有服务主密钥派生续页专用密钥。
func NewSyncSnapshotCursorSigner(masterKey []byte) (*SyncSnapshotCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("同步快照续页签名需要至少 32 字节主密钥")
	}

	// 2. 用快照续页专用用途标签派生本组件持有的密钥。
	return &SyncSnapshotCursorSigner{signingKey: syncSnapshotTokenMAC(masterKey, syncSnapshotCursorKeyPurpose)}, nil
}

// Sign 使用缓存的绑定、分页给出的下一位置和缓存的固定到期时间签发凭据。
func (signer *SyncSnapshotCursorSigner) Sign(binding SyncSnapshotCacheBinding, nextPosition int, expiresAt time.Time) (string, error) {
	// 1. 校验缓存绑定、下一位置与能用 RFC3339 表达的到期时间。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("同步快照续页签发器尚未初始化")
	}
	expiresAt = expiresAt.UTC()
	if !binding.valid() || nextPosition <= 0 || nextPosition >= domain.MaxSyncSnapshotEntries || nextPosition%binding.PageSize != 0 ||
		expiresAt.Year() < 1000 || expiresAt.Year() > 9999 {
		return "", domain.ErrInvalidSyncSnapshotCursor
	}

	// 2. 用规范文本保留完整标识、16 字节随机代次和 UTC 到期时间精度。
	payload := syncSnapshotCursorPayload{
		Version: syncSnapshotCursorVersion, SnapshotID: binding.SnapshotID.String(),
		UserID: strconv.FormatUint(binding.UserID, 10), DeviceID: strconv.FormatUint(binding.DeviceID, 10),
		Generation: hex.EncodeToString(binding.Generation[:]), PageSize: binding.PageSize,
		NextPosition: nextPosition, ExpiresAt: expiresAt.Format(time.RFC3339Nano),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码同步快照续页凭据失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 3. 对载荷文本计算 HMAC，拼接载荷与签名两段无填充 Base64URL。
	token := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signer.cursorMAC(encodedPayload))
	if len(token) > domain.MaxSyncSnapshotTokenLength {
		return "", domain.ErrInvalidSyncSnapshotCursor
	}
	return token, nil
}

func (signer *SyncSnapshotCursorSigner) cursorMAC(encodedPayload string) []byte {
	return syncSnapshotTokenMAC(signer.signingKey, encodedPayload)
}

// syncSnapshotTokenMAC 在调用方为各自用途选择的密钥下计算 HMAC-SHA256。
func syncSnapshotTokenMAC(signingKey []byte, message string) []byte {
	messageMAC := hmac.New(sha256.New, signingKey)
	messageMAC.Write([]byte(message))
	return messageMAC.Sum(nil)
}
