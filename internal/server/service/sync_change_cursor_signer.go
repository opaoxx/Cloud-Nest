package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	syncChangeCursorKeyPurpose = "cloudnest/sync-change-pull/v1"
	syncChangeCursorVersion    = 1
)

// syncChangeCursorPayload 固定第 1 版增量游标的文本字段和编码顺序。
type syncChangeCursorPayload struct {
	Version    uint8  `json:"version"`
	UserID     string `json:"user_id"`
	DeviceID   string `json:"device_id"`
	Generation string `json:"generation"`
	Sequence   string `json:"sequence"`
}

// SyncChangeCursorSigner 持有增量拉取游标的专用 HMAC 密钥。
type SyncChangeCursorSigner struct {
	signingKey []byte
}

// NewSyncChangeCursorSigner 从服务主密钥派生增量拉取专用密钥。
func NewSyncChangeCursorSigner(masterKey []byte) (*SyncChangeCursorSigner, error) {
	// 1. 核对主密钥长度，按增量拉取用途派生签名密钥。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("增量拉取游标签名需要至少 32 字节主密钥")
	}
	return &SyncChangeCursorSigner{signingKey: syncSnapshotTokenMAC(masterKey, syncChangeCursorKeyPurpose)}, nil
}

// Sign 签发当前设备能够继续读取的排他起点，调用方负责确认位置已实际交付。
func (signer *SyncChangeCursorSigner) Sign(position domain.SyncChangeCursorPosition) (string, error) {
	// 1. 校验签发器、账号、设备和完整代次。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("增量拉取游标签发器尚未初始化")
	}
	if position.UserID == 0 || position.DeviceID == 0 || position.Generation == (domain.SyncCursorGeneration{}) {
		return "", domain.ErrInvalidSyncChangeCursor
	}

	// 2. 按固定字段编码代次和十进制序号。
	payload, err := json.Marshal(syncChangeCursorPayload{
		Version:    syncChangeCursorVersion,
		UserID:     strconv.FormatUint(position.UserID, 10),
		DeviceID:   strconv.FormatUint(position.DeviceID, 10),
		Generation: hex.EncodeToString(position.Generation[:]),
		Sequence:   strconv.FormatUint(position.Sequence, 10),
	})
	if err != nil {
		return "", fmt.Errorf("编码增量拉取游标失败: %w", err)
	}

	// 3. 用增量拉取专用密钥签名，交付两段无填充 Base64URL 文本。
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	encodedSignature := base64.RawURLEncoding.EncodeToString(syncSnapshotTokenMAC(signer.signingKey, encodedPayload))
	token := encodedPayload + "." + encodedSignature
	if len(token) > domain.MaxSyncSnapshotTokenLength {
		return "", domain.ErrInvalidSyncChangeCursor
	}
	return token, nil
}
