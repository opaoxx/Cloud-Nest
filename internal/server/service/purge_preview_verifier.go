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

// PurgePreviewVerifier 使用预检专用密钥验证签发的凭证。
type PurgePreviewVerifier struct {
	signer *PurgePreviewSigner
}

// NewPurgePreviewVerifier 复用签发器的密钥校验和用途密钥派生。
func NewPurgePreviewVerifier(masterKey []byte) (*PurgePreviewVerifier, error) {
	// 1. 派生永久清理预检专用密钥。
	signer, err := NewPurgePreviewSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &PurgePreviewVerifier{signer: signer}, nil
}

// Verify 校验凭证并返回签发时记录的子树指纹。
// userID 来自已认证身份，entryID 是当前目标，now 是确认时的检查时间。
func (verifier *PurgePreviewVerifier) Verify(userID, entryID uint64, digest string, now time.Time) ([sha256.Size]byte, error) {
	// 1. 检查校验参数和凭证长度。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return [sha256.Size]byte{}, fmt.Errorf("永久清理预检校验器尚未初始化")
	}
	if userID == 0 || entryID == 0 || now.IsZero() || digest == "" || len(digest) > domain.MaxPurgePreviewDigestLength {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}

	// 2. 拆分凭证，先验证签名，再解码载荷。
	encodedPayload, encodedMAC, found := strings.Cut(digest, ".")
	if !found || encodedPayload == "" || len(encodedMAC) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(signature) != sha256.Size {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}

	// 3. 核对版本、用户、目标和载荷格式，还原指纹与截止时间。
	var payload purgePreviewPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	if payload.Version != 1 || payload.UserID != strconv.FormatUint(userID, 10) || payload.EntryID != strconv.FormatUint(entryID, 10) {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	fingerprintBytes, err := base64.RawURLEncoding.Strict().DecodeString(payload.SubtreeFingerprint)
	if err != nil || len(fingerprintBytes) != sha256.Size {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	fingerprint := [sha256.Size]byte(fingerprintBytes)
	expiresAt, err := time.Parse(purgePreviewTimeLayout, payload.ExpiresAt)
	if err != nil {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}
	// 复用签发器从截止时间重建规范文本，拒绝额外字段、重复字段及其他编码。
	issuedAt := expiresAt.Add(-domain.PurgePreviewLifetime)
	canonical, err := verifier.signer.Sign(userID, entryID, fingerprint, issuedAt)
	if err != nil || canonical.PreviewDigest != digest {
		return [sha256.Size]byte{}, domain.ErrInvalidPurgePreviewDigest
	}

	// 4. 检查截止时间，成功后返回指纹。
	if !now.Before(expiresAt) {
		return [sha256.Size]byte{}, domain.ErrPurgePreviewExpired
	}
	return fingerprint, nil
}
