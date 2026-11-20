package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	purgePreviewKeyPurpose = "cloudnest/purge-preview/v1"
	purgePreviewTimeLayout = "2006-01-02T15:04:05.000000Z"
)

// purgePreviewPayload 按第 1 版字段顺序编码凭证校验所需的事实。
type purgePreviewPayload struct {
	Version            uint8  `json:"version"`
	UserID             string `json:"user_id"`
	EntryID            string `json:"entry_id"`
	SubtreeFingerprint string `json:"subtree_fingerprint"`
	ExpiresAt          string `json:"expires_at"`
}

// PurgePreviewSigner 持有永久清理预检专用的签名密钥。
type PurgePreviewSigner struct {
	signingKey []byte
}

// NewPurgePreviewSigner 从服务主密钥派生预检专用密钥。
func NewPurgePreviewSigner(masterKey []byte) (*PurgePreviewSigner, error) {
	// 1. 从主密钥派生永久清理预检专用的密钥。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("永久清理预检签名需要至少 32 字节主密钥")
	}
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte(purgePreviewKeyPurpose))
	return &PurgePreviewSigner{signingKey: derivation.Sum(nil)}, nil
}

// Sign 绑定用户、目标和子树指纹，签发五分钟有效的预检凭证。
func (signer *PurgePreviewSigner) Sign(userID, entryID uint64, fingerprint [sha256.Size]byte, issuedAt time.Time) (domain.PurgePreviewCredential, error) {
	// 2. 核对输入，将用户、目标、指纹和五分钟后的截止时间写入载荷。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return domain.PurgePreviewCredential{}, fmt.Errorf("永久清理预检签发器尚未初始化")
	}
	issuedAt = issuedAt.UTC().Truncate(time.Microsecond)
	if userID == 0 || entryID == 0 || issuedAt.Year() < 1000 || issuedAt.Year() > 9999 {
		return domain.PurgePreviewCredential{}, domain.ErrInvalidPurgePreviewDigest
	}
	expiresAt := issuedAt.Add(domain.PurgePreviewLifetime)
	if expiresAt.Year() > 9999 {
		return domain.PurgePreviewCredential{}, domain.ErrInvalidPurgePreviewDigest
	}
	payload := purgePreviewPayload{
		Version:            1,
		UserID:             strconv.FormatUint(userID, 10),
		EntryID:            strconv.FormatUint(entryID, 10),
		SubtreeFingerprint: base64.RawURLEncoding.EncodeToString(fingerprint[:]),
		ExpiresAt:          expiresAt.Format(purgePreviewTimeLayout),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return domain.PurgePreviewCredential{}, fmt.Errorf("编码永久清理预检凭证失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 3. 用 HMAC-SHA256 签名，返回凭证和同一个截止时间。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	digest := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	if len(digest) > domain.MaxPurgePreviewDigestLength {
		return domain.PurgePreviewCredential{}, domain.ErrInvalidPurgePreviewDigest
	}
	return domain.PurgePreviewCredential{PreviewDigest: digest, ExpiresAt: expiresAt}, nil
}
