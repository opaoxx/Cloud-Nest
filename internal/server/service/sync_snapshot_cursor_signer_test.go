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

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncSnapshotCursorSignerCanonicalToken(t *testing.T) {
	// 1. 准备完整无符号标识和纳秒期限，用实际输出核对公开文本形状。
	masterKey := bytes.Repeat([]byte{0x53}, sha256.Size)
	signer, err := NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := syncSnapshotCacheFixture()
	binding.UserID, binding.DeviceID = math.MaxUint64, math.MaxUint64-1
	expiresAt := time.Date(2026, 11, 23, 10, 10, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	token, err := signer.Sign(binding, 50, expiresAt)
	if err != nil || len(token) > 1024 || strings.Count(token, ".") != 1 || strings.Contains(token, "=") {
		t.Fatalf("续页凭据形状错误，错误=%v", err)
	}
	payloadText, signatureText, _ := strings.Cut(token, ".")
	payload, err := base64.RawURLEncoding.Strict().DecodeString(payloadText)
	if err != nil {
		t.Fatal(err)
	}
	const wantPayload = `{"version":1,"snapshot_id":"052dc4f7-f4c1-46b3-a509-bf96992f6580","user_id":"18446744073709551615","device_id":"18446744073709551614","generation":"f10203040506070809101112131415fe","page_size":50,"next_position":50,"expires_at":"2026-11-23T02:10:00.123456789Z"}`
	if string(payload) != wantPayload {
		t.Fatalf("凭据载荷不符，实际=%s", payload)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(signatureText)
	if err != nil || len(signature) != sha256.Size {
		t.Fatalf("签名长度错误，错误=%v", err)
	}

	// 2. 独立计算签名，验证快照用途和完整载荷都参与计算。
	checkMAC := func(purpose, encodedPayload string) []byte {
		derived := hmac.New(sha256.New, masterKey)
		derived.Write([]byte(purpose))
		mac := hmac.New(sha256.New, derived.Sum(nil))
		mac.Write([]byte(encodedPayload))
		return mac.Sum(nil)
	}
	const purpose = "cloudnest/sync-snapshot-cursor/v1"
	tampered := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(wantPayload, `"next_position":50`, `"next_position":100`, 1)))
	if !hmac.Equal(signature, checkMAC(purpose, payloadText)) || hmac.Equal(signature, checkMAC(purpose, tampered)) ||
		hmac.Equal(signature, checkMAC("cloudnest/directory-cursor/v1", payloadText)) {
		t.Fatal("签名未绑定规范载荷或未按用途隔离")
	}

	// 3. 缓存的同一到期时刻应产生相同凭据，调用方修改主密钥切片不影响已派生密钥。
	clear(masterKey)
	again, err := signer.Sign(binding, 50, expiresAt.UTC())
	if err != nil || again != token {
		t.Fatalf("重读时签发的凭据发生变化，错误=%v", err)
	}
	if changed, err := signer.Sign(binding, 50, expiresAt.Add(time.Nanosecond)); err != nil || changed == token {
		t.Fatalf("期限相差 1 纳秒时签名应发生变化，错误=%v", err)
	}
}

func TestSyncSnapshotCursorSignerPositionBounds(t *testing.T) {
	// 1. 允许最小页大小、最大页大小，以及 10000 项范围内最后一个可能的续页位置。
	signer, err := NewSyncSnapshotCursorSigner(bytes.Repeat([]byte{0x29}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := syncSnapshotCacheFixture()
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
	for _, testCase := range []struct{ pageSize, position int }{{1, 1}, {1, 9999}, {100, 9900}} {
		binding.PageSize = testCase.pageSize
		if value, err := signer.Sign(binding, testCase.position, expiresAt); err != nil || value == "" {
			t.Fatalf("有效的续页位置 %d 应可签发，错误=%v", testCase.position, err)
		}
	}
}

func TestSyncSnapshotCursorSignerRejectsInvalidInput(t *testing.T) {
	// 1. 主密钥过短或签发器未初始化时拒绝签发。
	if signer, err := NewSyncSnapshotCursorSigner(make([]byte, sha256.Size-1)); err == nil || signer != nil {
		t.Fatal("主密钥过短时应初始化失败")
	}
	binding, _ := syncSnapshotCacheFixture()
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
	var uninitialized SyncSnapshotCursorSigner
	if value, err := uninitialized.Sign(binding, 50, expiresAt); value != "" || err == nil {
		t.Fatal("未初始化的签发器应拒绝签发")
	}
	signer, err := NewSyncSnapshotCursorSigner(bytes.Repeat([]byte{0x29}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}

	// 2. 无效绑定、非法页边界及不可表达的期限都不得产生凭据。
	for _, testCase := range []struct {
		name   string
		change func(*SyncSnapshotCacheBinding, *int, *time.Time)
	}{
		{"zero snapshot ID", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) { key.SnapshotID = uuid.Nil }},
		{"wrong UUID variant", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) { key.SnapshotID[8] = 0 }},
		{"zero user", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) { key.UserID = 0 }},
		{"zero device", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) { key.DeviceID = 0 }},
		{"zero generation", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) {
			key.Generation = domain.SyncCursorGeneration{}
		}},
		{"zero page size", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) { key.PageSize = 0 }},
		{"page size too large", func(key *SyncSnapshotCacheBinding, _ *int, _ *time.Time) { key.PageSize = 101 }},
		{"initial position", func(_ *SyncSnapshotCacheBinding, position *int, _ *time.Time) { *position = 0 }},
		{"negative position", func(_ *SyncSnapshotCacheBinding, position *int, _ *time.Time) { *position = -50 }},
		{"unaligned position", func(_ *SyncSnapshotCacheBinding, position *int, _ *time.Time) { *position = 51 }},
		{"position too large", func(_ *SyncSnapshotCacheBinding, position *int, _ *time.Time) { *position = 10000 }},
		{"zero expiration", func(_ *SyncSnapshotCacheBinding, _ *int, stamp *time.Time) { *stamp = time.Time{} }},
		{"expiration year overflow in UTC", func(_ *SyncSnapshotCacheBinding, _ *int, stamp *time.Time) {
			*stamp = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			key, position, expiry := binding, 50, expiresAt
			testCase.change(&key, &position, &expiry)
			if value, err := signer.Sign(key, position, expiry); value != "" || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
				t.Fatalf("异常输入应拒绝签发，错误=%v", err)
			}
		})
	}
}
