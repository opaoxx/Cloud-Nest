package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncChangeCursorVerifierRoundTrip(t *testing.T) {
	// 1. 独立构造签发器和验签器，核对零位置、完整 uint64 和 16 字节代次。
	masterKey := bytes.Repeat([]byte{0x76}, sha256.Size)
	signer, err := NewSyncChangeCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []uint64{0, 17, math.MaxUint64} {
		want := domain.SyncChangeCursorPosition{
			UserID: math.MaxUint64, DeviceID: math.MaxUint64 - 1, Sequence: sequence,
			Generation: domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe},
		}
		token, err := signer.Sign(want)
		if err != nil {
			t.Fatal(err)
		}
		// 2. 同一凭据可反复验签，每次返回独立的位置副本。
		for range 2 {
			result, err := verifier.Verify(want.UserID, want.DeviceID, token)
			if err != nil || result == nil || *result != want {
				t.Fatalf("验签结果不符：%+v，错误 %v", result, err)
			}
			result.Generation[15] ^= 1
		}
	}
}

func TestSyncChangeCursorVerifierRejectsWrongBindingAndSignature(t *testing.T) {
	// 1. 不同身份、前置条件异常和错误密钥不得交付任何位置。
	masterKey := bytes.Repeat([]byte{0x45}, sha256.Size)
	verifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := syncSnapshotCacheFixture()
	position := domain.SyncChangeCursorPosition{UserID: binding.UserID, DeviceID: binding.DeviceID, Generation: binding.Generation, Sequence: 17}
	token, err := verifier.signer.Sign(position)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]uint64{{0, position.DeviceID}, {position.UserID, 0}, {position.UserID + 1, position.DeviceID}, {position.UserID, position.DeviceID + 1}} {
		if result, err := verifier.Verify(identity[0], identity[1], token); result != nil || !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
			t.Fatalf("错误身份仍通过校验：%+v，错误 %v", result, err)
		}
	}
	if invalid, err := NewSyncChangeCursorVerifier(make([]byte, sha256.Size-1)); invalid != nil || err == nil {
		t.Fatal("验签器接受了过短的密钥")
	}
	var uninitialized SyncChangeCursorVerifier
	if result, err := uninitialized.Verify(position.UserID, position.DeviceID, token); result != nil || err == nil {
		t.Fatal("未初始化的验签器交付了位置")
	}
	wrongKey, err := NewSyncChangeCursorVerifier(bytes.Repeat([]byte{0x46}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := wrongKey.Verify(position.UserID, position.DeviceID, token); result != nil || !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
		t.Fatalf("错误密钥未拒绝凭据：%v", err)
	}

	// 2. 测试真实的快照凭据、篡改和不符合外层格式的文本。
	pageSigner, err := NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	baselineSigner, err := NewSyncSnapshotBaselineSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
	pageCursor, err := pageSigner.Sign(binding, binding.PageSize, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	baselineToken, err := baselineSigner.Sign(binding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	message, signature, _ := strings.Cut(token, ".")
	for _, candidate := range []string{"", " ", strings.Repeat("a", domain.MaxSyncSnapshotTokenLength+1), token + ".extra",
		message + "=." + signature, token + "=", message + "A." + signature, pageCursor, baselineToken} {
		if result, err := verifier.Verify(position.UserID, position.DeviceID, candidate); result != nil || !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
			t.Fatalf("异常游标仍通过校验：%v", err)
		}
	}
}

func TestSyncChangeCursorVerifierRejectsSignedNoncanonicalPayload(t *testing.T) {
	// 1. 用测试密钥签发非规范载荷，确保正确签名也须通过格式和字段校验。
	verifier, err := NewSyncChangeCursorVerifier(bytes.Repeat([]byte{0x80}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	const generation = "f100030400060708090a0b0c0d0e0ffe"
	const canonical = `{"version":1,"user_id":"7","device_id":"19","generation":"` + generation + `","sequence":"0"}`
	for _, testCase := range []struct{ name, original, replacement string }{
		{"version", `"version":1`, `"version":2`},
		{"extra field", `"version":1`, `"version":1,"extra":0`},
		{"duplicate field", `"version":1`, `"version":1,"version":1`},
		{"reordered keys", `"user_id":"7","device_id":"19"`, `"device_id":"19","user_id":"7"`},
		{"leading whitespace", `{`, `{ `},
		{"alternate key case", `"sequence":"0"`, `"Sequence":"0"`},
		{"noncanonical user", `"user_id":"7"`, `"user_id":"07"`},
		{"numeric device", `"device_id":"19"`, `"device_id":19`},
		{"uppercase generation", generation, strings.ToUpper(generation)},
		{"invalid hex", generation, "z" + generation[1:]},
		{"zero generation", generation, strings.Repeat("0", 32)},
		{"short generation", generation, generation[:30]},
		{"missing sequence", `,"sequence":"0"`, ``},
		{"numeric sequence", `"sequence":"0"`, `"sequence":0`},
		{"null sequence", `"sequence":"0"`, `"sequence":null`},
		{"leading zero sequence", `"sequence":"0"`, `"sequence":"00"`},
		{"unsigned overflow", `"sequence":"0"`, `"sequence":"18446744073709551616"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			payload := strings.Replace(canonical, testCase.original, testCase.replacement, 1)
			if payload == canonical {
				t.Fatal("非规范测试载荷未修改")
			}
			message := base64.RawURLEncoding.EncodeToString([]byte(payload))
			token := message + "." + base64.RawURLEncoding.EncodeToString(syncSnapshotTokenMAC(verifier.signer.signingKey, message))
			if result, err := verifier.Verify(7, 19, token); result != nil || !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
				t.Fatalf("带正确签名的非规范载荷未拒绝：%v", err)
			}
		})
	}
}
