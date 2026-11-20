package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestPurgePreviewVerifierExpiryAndScope(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, sha256.Size)
	signer, err := NewPurgePreviewSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewPurgePreviewVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongVerifier, err := NewPurgePreviewVerifier(bytes.Repeat([]byte{8}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}

	// 1. 用独立初始化的校验器还原签发的指纹，保留完整无符号 ID。
	const userID, entryID = uint64(math.MaxUint64), uint64(math.MaxUint64 - 1)
	issuedAt := time.Date(2026, 11, 20, 12, 30, 0, 123456789, time.FixedZone("UTC+08", 8*3600))
	fingerprint := sha256.Sum256([]byte("fixture subtree"))
	credential, err := signer.Sign(userID, entryID, fingerprint, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := credential.ExpiresAt.In(issuedAt.Location())
	checkTime := expiresAt.Add(-time.Nanosecond)
	if actual, err := verifier.Verify(userID, entryID, credential.PreviewDigest, checkTime); err != nil || actual != fingerprint {
		t.Fatalf("截止前应还原原指纹，实际 %x，错误 %v", actual, err)
	}

	// 2. 恰好到期即失效，错误用户和目标始终返回无效凭证。
	for _, scenario := range []struct {
		name            string
		verifier        *PurgePreviewVerifier
		userID, entryID uint64
		now             time.Time
		wantError       error
	}{
		{"at expiry", verifier, userID, entryID, expiresAt, domain.ErrPurgePreviewExpired},
		{"after expiry", verifier, userID, entryID, expiresAt.Add(time.Nanosecond), domain.ErrPurgePreviewExpired},
		{"wrong user", verifier, userID - 1, entryID, expiresAt, domain.ErrInvalidPurgePreviewDigest},
		{"wrong target", verifier, userID, entryID - 1, expiresAt, domain.ErrInvalidPurgePreviewDigest},
		{"wrong key", wrongVerifier, userID, entryID, checkTime, domain.ErrInvalidPurgePreviewDigest},
		{"zero user", verifier, 0, entryID, checkTime, domain.ErrInvalidPurgePreviewDigest},
		{"zero target", verifier, userID, 0, checkTime, domain.ErrInvalidPurgePreviewDigest},
		{"zero clock", verifier, userID, entryID, time.Time{}, domain.ErrInvalidPurgePreviewDigest},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actual, err := scenario.verifier.Verify(scenario.userID, scenario.entryID, credential.PreviewDigest, scenario.now)
			if actual != ([sha256.Size]byte{}) || !errors.Is(err, scenario.wantError) {
				t.Fatalf("期望空指纹及 %v，实际 %x，错误 %v", scenario.wantError, actual, err)
			}
		})
	}

	// 3. 保留签发器支持的时间下界与完整指纹值域。
	earliest := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
	zeroFingerprint := [sha256.Size]byte{}
	earliestCredential, err := signer.Sign(userID, entryID, zeroFingerprint, earliest)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := verifier.Verify(userID, entryID, earliestCredential.PreviewDigest, earliest); err != nil || actual != zeroFingerprint {
		t.Fatalf("有效凭证应保留全零指纹，实际 %x，错误 %v", actual, err)
	}
}

func TestPurgePreviewVerifierRejectsInvalidDigest(t *testing.T) {
	if verifier, err := NewPurgePreviewVerifier(make([]byte, sha256.Size-1)); verifier != nil || err == nil {
		t.Fatal("校验器应拒绝短主密钥", err)
	}
	verifier, err := NewPurgePreviewVerifier(bytes.Repeat([]byte{9}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	for _, invalidVerifier := range []*PurgePreviewVerifier{nil, {}} {
		if actual, err := invalidVerifier.Verify(7, 14, "fixture", now); actual != ([sha256.Size]byte{}) || err == nil {
			t.Fatalf("未初始化校验器应拒绝请求，实际 %x，错误 %v", actual, err)
		}
	}

	// 1. 修改真实签发的凭证，检查结构和签名。
	fingerprint := sha256.Sum256([]byte("fixture subtree"))
	credential, err := verifier.signer.Sign(7, 14, fingerprint, now)
	if err != nil {
		t.Fatal(err)
	}
	encodedPayload, encodedMAC, _ := strings.Cut(credential.PreviewDigest, ".")
	changedMAC := []byte(encodedMAC)
	changedMAC[0] ^= 32
	for _, scenario := range []struct {
		name, digest string
	}{
		{"empty", ""},
		{"too long", strings.Repeat("a", domain.MaxPurgePreviewDigestLength+1)},
		{"missing separator", encodedPayload},
		{"empty payload", "." + encodedMAC},
		{"extra separator", credential.PreviewDigest + "."},
		{"padded MAC", credential.PreviewDigest + "="},
		{"changed payload", "A" + encodedPayload[1:] + "." + encodedMAC},
		{"changed MAC", encodedPayload + "." + string(changedMAC)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actual, err := verifier.Verify(7, 14, scenario.digest, now)
			if actual != ([sha256.Size]byte{}) || !errors.Is(err, domain.ErrInvalidPurgePreviewDigest) {
				t.Fatalf("应拒绝异常凭证，实际 %x，错误 %v", actual, err)
			}
		})
	}

	// 2. 用测试密钥构造带正确签名的异常载荷，单独验证载荷规则。
	const canonicalBody = `{"version":1,"user_id":"7","entry_id":"14","subtree_fingerprint":"Y87MNS1_SgZwJMBTWC5ZHl3k5lz-uVGk40nkQrfC52M","expires_at":"2026-11-20T04:35:00.123456Z"}`
	for _, scenario := range []struct {
		name, body string
	}{
		{"invalid JSON", "{"},
		{"unknown version", strings.Replace(canonicalBody, `"version":1`, `"version":2`, 1)},
		{"unknown field", `{"extra":true,` + canonicalBody[1:]},
		{"duplicate field", `{"version":1,` + canonicalBody[1:]},
		{"noncanonical ID", strings.Replace(canonicalBody, `"entry_id":"14"`, `"entry_id":"014"`, 1)},
		{"short fingerprint", strings.Replace(canonicalBody, "Y87MNS1_SgZwJMBTWC5ZHl3k5lz-uVGk40nkQrfC52M", "AA", 1)},
		{"noncanonical time", strings.Replace(canonicalBody, ".123456Z", ",123456Z", 1)},
		{"unsignable expiry", strings.Replace(canonicalBody, "2026-11-20T04:35:00", "1000-01-01T00:04:59", 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			encoded := base64.RawURLEncoding.EncodeToString([]byte(scenario.body))
			messageMAC := hmac.New(sha256.New, verifier.signer.signingKey)
			messageMAC.Write([]byte(encoded))
			digest := encoded + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
			actual, err := verifier.Verify(7, 14, digest, now)
			if actual != ([sha256.Size]byte{}) || !errors.Is(err, domain.ErrInvalidPurgePreviewDigest) {
				t.Fatalf("应拒绝带正确签名的异常载荷，实际 %x，错误 %v", actual, err)
			}
		})
	}
}
