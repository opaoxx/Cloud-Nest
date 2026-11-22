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

func syncSnapshotCursorVerificationFixture(t *testing.T) (*SyncSnapshotCursorVerifier, SyncSnapshotCacheBinding, string, time.Time) {
	t.Helper()
	verifier, err := NewSyncSnapshotCursorVerifier(bytes.Repeat([]byte{0x34}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := syncSnapshotCacheFixture()
	binding.UserID, binding.DeviceID = math.MaxUint64, math.MaxUint64-1
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 123456789, time.UTC)
	cursor, err := verifier.signer.Sign(binding, 100, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return verifier, binding, cursor, expiresAt
}

func TestSyncSnapshotCursorVerifierRoundTripAndExpiry(t *testing.T) {
	// 1. 到期前 1 纳秒可重复校验，完整返回绑定、位置和原到期时间。
	verifier, binding, cursor, expiresAt := syncSnapshotCursorVerificationFixture(t)
	for range 2 {
		got, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, cursor, expiresAt.Add(-time.Nanosecond))
		want := VerifiedSyncSnapshotCursor{Binding: binding, NextPosition: 100, ExpiresAt: expiresAt}
		if err != nil || got == nil || *got != want {
			t.Fatalf("校验结果或时间精度错误，结果=%+v，错误=%v", got, err)
		}
	}

	// 2. 同一设备的可信凭据在达到到期时刻后返回失效。
	for _, now := range []time.Time{expiresAt, expiresAt.Add(time.Nanosecond)} {
		if got, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, cursor, now); got != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
			t.Fatalf("在 %s 应返回凭据失效，错误=%v", now, err)
		}
	}
}

func TestSyncSnapshotCursorVerifierRejectsWrongBindingAndSignature(t *testing.T) {
	// 1. 即使已到期，也先拒绝错误的账号、设备和路径绑定。
	verifier, binding, cursor, expiresAt := syncSnapshotCursorVerificationFixture(t)
	otherSnapshot := binding
	otherSnapshot.SnapshotID[15] ^= 1
	for _, expected := range []SyncSnapshotCacheBinding{
		{UserID: binding.UserID - 1, DeviceID: binding.DeviceID, SnapshotID: binding.SnapshotID},
		{UserID: binding.UserID, DeviceID: binding.DeviceID - 1, SnapshotID: binding.SnapshotID},
		otherSnapshot,
	} {
		if got, err := verifier.Verify(expected.UserID, expected.DeviceID, expected.SnapshotID, cursor, expiresAt); got != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
			t.Fatalf("绑定错误应返回凭据无效，错误=%v", err)
		}
	}

	// 2. 通过合成密钥验证用途隔离，改动凭据内容时不返回绑定数据。
	otherPurpose, err := NewDirectoryCursorSigner(bytes.Repeat([]byte{0x34}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	directoryToken, err := otherPurpose.Sign(binding.UserID, 11, domain.FileEntryPosition{ID: 12, ModifiedAt: expiresAt.Truncate(time.Microsecond)})
	if err != nil {
		t.Fatal(err)
	}
	encodedPayload, encodedMAC, _ := strings.Cut(cursor, ".")
	for _, candidate := range []string{"", " ", strings.Repeat("a", domain.MaxSyncSnapshotTokenLength+1), cursor + ".extra",
		encodedPayload + "=." + encodedMAC, encodedPayload + "." + encodedMAC + "=", "A" + cursor[1:], directoryToken} {
		if got, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, candidate, expiresAt); got != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
			t.Fatalf("异常凭据应返回无效，错误=%v", err)
		}
	}
}

func TestSyncSnapshotCursorVerifierRejectsSignedNoncanonicalPayload(t *testing.T) {
	// 1. 使用测试专用密钥分别给异常载荷签名，确认格式校验独立生效。
	verifier, binding, cursor, expiresAt := syncSnapshotCursorVerificationFixture(t)
	encoded, _, _ := strings.Cut(cursor, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ name, old, replacement string }{
		{"version", `"version":1`, `"version":2`},
		{"unknown field", `"version":1`, `"version":1,"extra":0`},
		{"duplicate field", `"version":1`, `"version":1,"version":1`},
		{"case-folded field", `"user_id":`, `"USER_ID":`},
		{"whitespace", `"version":1`, `"version": 1`},
		{"uppercase generation", `"generation":"f10203040506070809101112131415fe"`, `"generation":"F10203040506070809101112131415FE"`},
		{"short generation", `"generation":"f10203040506070809101112131415fe"`, `"generation":"f102030405060708"`},
		{"oversized generation", `"generation":"f10203040506070809101112131415fe"`, `"generation":"f10203040506070809101112131415feff"`},
		{"zero generation", `"generation":"f10203040506070809101112131415fe"`, `"generation":"00000000000000000000000000000000"`},
		{"position not aligned", `"next_position":100`, `"next_position":101`},
		{"page size not allowed", `"page_size":50`, `"page_size":0`},
		{"offset timestamp", `2026-11-23T02:10:00.123456789Z`, `2026-11-23T10:10:00.123456789+08:00`},
		{"null date", `"2026-11-23T02:10:00.123456789Z"`, `null`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := strings.Replace(string(payload), testCase.old, testCase.replacement, 1)
			if changed == string(payload) {
				t.Fatal("测试载荷未修改")
			}
			message := base64.RawURLEncoding.EncodeToString([]byte(changed))
			candidate := message + "." + base64.RawURLEncoding.EncodeToString(verifier.signer.cursorMAC(message))
			if got, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, candidate, expiresAt.Add(-time.Nanosecond)); got != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
				t.Fatalf("非规范凭据应返回无效，错误=%v", err)
			}
		})
	}
}
