package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// syncSnapshotTokenMAC 在调用方为各自用途选择的密钥下计算 HMAC-SHA256。
func syncSnapshotTokenMAC(signingKey []byte, message string) []byte {
	messageMAC := hmac.New(sha256.New, signingKey)
	messageMAC.Write([]byte(message))
	return messageMAC.Sum(nil)
}

// readSyncSnapshotTokenPayload 用指定用途密钥验签，再解码已经认证的载荷。
func readSyncSnapshotTokenPayload(signingKey []byte, token string) ([]byte, bool) {
	// 1. 限定输入长度及签名形状。
	if len(token) > domain.MaxSyncSnapshotTokenLength {
		return nil, false
	}
	encodedPayload, encodedMAC, found := strings.Cut(token, ".")
	if !found || encodedPayload == "" || len(encodedMAC) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return nil, false
	}
	// 2. 先核对签名，再解码载荷。
	signature, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || !hmac.Equal(signature, syncSnapshotTokenMAC(signingKey, encodedPayload)) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	return payload, err == nil
}
