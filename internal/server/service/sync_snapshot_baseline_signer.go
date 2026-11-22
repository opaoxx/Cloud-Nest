package service

import (
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
	syncSnapshotBaselineKeyPurpose = "cloudnest/sync-snapshot-baseline/v1"
	syncSnapshotBaselineVersion    = 1
)

// syncSnapshotBaselinePayload 固定第 1 版末页确认凭据的编码顺序。
type syncSnapshotBaselinePayload struct {
	Version           uint8  `json:"version"`
	SnapshotID        string `json:"snapshot_id"`
	UserID            string `json:"user_id"`
	DeviceID          string `json:"device_id"`
	Generation        string `json:"generation"`
	HighWaterSequence string `json:"high_water_sequence"`
	ExpiresAt         string `json:"expires_at"`
}

// SyncSnapshotBaselineSigner 持有末页确认专用的 HMAC 密钥。
type SyncSnapshotBaselineSigner struct {
	signingKey []byte
}

// NewSyncSnapshotBaselineSigner 从服务主密钥派生末页确认专用密钥。
func NewSyncSnapshotBaselineSigner(masterKey []byte) (*SyncSnapshotBaselineSigner, error) {
	// 1. 核对主密钥长度，再按用途派生独立的签名密钥。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("同步快照末页确认签名需要至少 32 字节主密钥")
	}
	return &SyncSnapshotBaselineSigner{signingKey: syncSnapshotTokenMAC(masterKey, syncSnapshotBaselineKeyPurpose)}, nil
}

// Sign 用原快照的绑定、截止序号和固定到期时刻签发末页确认凭据。
// 调用方在交付末页或空树时使用，截止序号从缓存头部取得。
func (signer *SyncSnapshotBaselineSigner) Sign(binding SyncSnapshotCacheBinding, highWaterSequence uint64, expiresAt time.Time) (string, error) {
	// 1. 核对签发器、原快照绑定和能用 RFC3339 表达的到期时刻。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("同步快照末页确认签发器尚未初始化")
	}
	expiresAt = expiresAt.UTC()
	if !binding.validIdentity() || expiresAt.Year() < 1000 || expiresAt.Year() > 9999 {
		return "", domain.ErrInvalidSyncSnapshotBaselineToken
	}

	// 2. 编码完整的设备代次和截止序号，序号零是有效的同步基线。
	payload := syncSnapshotBaselinePayload{
		Version: syncSnapshotBaselineVersion, SnapshotID: binding.SnapshotID.String(),
		UserID: strconv.FormatUint(binding.UserID, 10), DeviceID: strconv.FormatUint(binding.DeviceID, 10),
		Generation: hex.EncodeToString(binding.Generation[:]), HighWaterSequence: strconv.FormatUint(highWaterSequence, 10),
		ExpiresAt: expiresAt.Format(time.RFC3339Nano),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码同步快照末页确认凭据失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 3. 签名后拼接载荷和签名，形成两段无填充 Base64URL 文本。
	token := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(syncSnapshotTokenMAC(signer.signingKey, encodedPayload))
	if len(token) > domain.MaxSyncSnapshotTokenLength {
		return "", domain.ErrInvalidSyncSnapshotBaselineToken
	}
	return token, nil
}
