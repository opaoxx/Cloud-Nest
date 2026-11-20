package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestPurgePreviewSignerFixedVector(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, sha256.Size)
	signer, err := NewPurgePreviewSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := BuildPurgePreviewSubtreeFingerprint([]domain.PurgePreviewSubtreeEntry{
		{ID: 14, Revision: 1, EntryType: "DIR", Status: "TRASHED"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. 使用带时区及纳秒的时间，核对截止时间的 UTC 微秒表示。
	issuedAt := time.Date(2026, 11, 20, 12, 30, 0, 123456789, time.FixedZone("UTC+08", 8*3600))
	credential, err := signer.Sign(7, 14, fingerprint, issuedAt)
	wantExpiry := time.Date(2026, 11, 20, 4, 35, 0, 123456000, time.UTC)
	if err != nil || credential.ExpiresAt != wantExpiry || domain.PurgePreviewLifetime != 5*time.Minute {
		t.Fatal("签发结果或五分钟有效期不符", credential, err)
	}

	// 2. 核对契约格式、载荷原文和独立 Python 计算的签名。
	pattern := regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$`)
	if !pattern.MatchString(credential.PreviewDigest) || len(credential.PreviewDigest) > domain.MaxPurgePreviewDigestLength {
		t.Fatal("预检签名不符合 Base64URL 契约")
	}
	encoded, signature, _ := strings.Cut(credential.PreviewDigest, ".")
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	const wantPayload = `{"version":1,"user_id":"7","entry_id":"14","subtree_fingerprint":"Y87MNS1_SgZwJMBTWC5ZHl3k5lz-uVGk40nkQrfC52M","expires_at":"2026-11-20T04:35:00.123456Z"}`
	const wantSignature = "kgDANATTiApaNoTyyNFAfH9b3GuKYmUqrPaZMkHZdEo"
	if err != nil || string(payload) != wantPayload || signature != wantSignature {
		t.Fatal("载荷字段、编码或固定签名不符", string(payload), signature, err)
	}

	// 3. 核对用途分离及签发器对主密钥输入的独立持有。
	cursorKey := hmac.New(sha256.New, masterKey)
	cursorKey.Write([]byte(trashEntryCursorKeyPurpose))
	cursorMAC := hmac.New(sha256.New, cursorKey.Sum(nil))
	cursorMAC.Write([]byte(encoded))
	masterMAC := hmac.New(sha256.New, masterKey)
	masterMAC.Write([]byte(encoded))
	if signature == base64.RawURLEncoding.EncodeToString(cursorMAC.Sum(nil)) ||
		signature == base64.RawURLEncoding.EncodeToString(masterMAC.Sum(nil)) {
		t.Fatal("预检凭证与回收站游标或主密钥直接签名混用")
	}
	clear(masterKey)
	repeated, err := signer.Sign(7, 14, fingerprint, issuedAt.UTC())
	if err != nil || repeated != credential {
		t.Fatal("同一时刻的重算改变了凭证", err)
	}
}

func TestPurgePreviewSignerBindingAndBounds(t *testing.T) {
	signer, err := NewPurgePreviewSigner(bytes.Repeat([]byte{1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	const userID = uint64(math.MaxUint64)
	const entryID = uint64(math.MaxUint64 - 1)
	fingerprint := sha256.Sum256([]byte("fixture subtree"))
	issuedAt := time.Date(9999, 12, 31, 23, 54, 59, 999999000, time.UTC)
	original, err := signer.Sign(userID, entryID, fingerprint, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	encoded, originalSignature, _ := strings.Cut(original.PreviewDigest, ".")
	body, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var fields purgePreviewPayload
	if err := json.Unmarshal(body, &fields); err != nil || fields.UserID != "18446744073709551615" ||
		fields.EntryID != "18446744073709551614" || fields.ExpiresAt != "9999-12-31T23:59:59.999999Z" {
		t.Fatal("最大标识或 UTC 截止时间未完整保留", fields, err)
	}
	for _, scenario := range []struct {
		name            string
		userID, entryID uint64
		fingerprint     [sha256.Size]byte
		issuedAt        time.Time
	}{
		{"different user", userID - 1, entryID, fingerprint, issuedAt},
		{"different target", userID, entryID + 1, fingerprint, issuedAt},
		{"different fingerprint", userID, entryID, sha256.Sum256([]byte("changed subtree")), issuedAt},
		{"different expiration", userID, entryID, fingerprint, issuedAt.Add(-time.Microsecond)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			credential, err := signer.Sign(scenario.userID, scenario.entryID, scenario.fingerprint, scenario.issuedAt)
			_, signature, _ := strings.Cut(credential.PreviewDigest, ".")
			if err != nil || signature == originalSignature {
				t.Fatal("签名未绑定预检事实", err)
			}
		})
	}
}

func TestPurgePreviewSignerInvalidInput(t *testing.T) {
	if signer, err := NewPurgePreviewSigner(make([]byte, sha256.Size-1)); signer != nil || err == nil {
		t.Fatal("短主密钥未被拒绝", err)
	}
	signer, err := NewPurgePreviewSigner(make([]byte, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 4, 30, 0, 0, time.UTC)
	fingerprint := sha256.Sum256([]byte("fixture"))
	for _, scenario := range []struct {
		userID, entryID uint64
		issuedAt        time.Time
	}{
		{0, 14, now},
		{7, 0, now},
		{7, 14, time.Time{}},
		{7, 14, time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC+01", 3600))},
		{7, 14, time.Date(9999, 12, 31, 23, 55, 0, 0, time.UTC)},
		{7, 14, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		credential, err := signer.Sign(scenario.userID, scenario.entryID, fingerprint, scenario.issuedAt)
		if credential != (domain.PurgePreviewCredential{}) || !errors.Is(err, domain.ErrInvalidPurgePreviewDigest) {
			t.Fatal("无效签发输入产生了结果", scenario, credential, err)
		}
	}
	for _, invalidSigner := range []*PurgePreviewSigner{nil, {}} {
		if credential, err := invalidSigner.Sign(7, 14, fingerprint, now); credential != (domain.PurgePreviewCredential{}) || err == nil {
			t.Fatal("未初始化签发器产生了结果", credential, err)
		}
	}
}
