package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncSnapshotBaselineSignerCanonicalToken(t *testing.T) {
	// 1. 读取缓存的实际末页，使用原截止序号和到期时间签发凭据。
	masterKey := bytes.Repeat([]byte{0x98}, sha256.Size)
	signer, err := NewSyncSnapshotBaselineSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name     string
		sequence uint64
		want     string
	}{
		{"empty tree at zero", 0, "0"},
		{"full unsigned cutoff", math.MaxUint64, "18446744073709551615"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
			cache := NewSyncSnapshotCache(func() time.Time { return now })
			binding, data := syncSnapshotCacheFixture()
			binding.UserID, binding.DeviceID = math.MaxUint64, math.MaxUint64-1
			data.Header.HighWaterSequence = testCase.sequence
			if testCase.sequence == 0 {
				data.Entries = domain.SyncSnapshotEntriesJSON{Items: []json.RawMessage{}, SizeBytes: 2}
			}
			expiresAt := putSyncSnapshotForReadTest(t, cache, binding, data)
			snapshot, err := cache.Get(t.Context(), binding)
			if err != nil {
				t.Fatal(err)
			}
			page, err := snapshot.Page(0)
			if err != nil || page.NextPosition != nil {
				t.Fatalf("夹具应只有末页，错误=%v", err)
			}
			token, err := signer.Sign(binding, snapshot.Header.HighWaterSequence, snapshot.ExpiresAt)
			if err != nil || len(token) > domain.MaxSyncSnapshotTokenLength || strings.Count(token, ".") != 1 || strings.Contains(token, "=") {
				t.Fatalf("末页确认凭据的公开格式错误，错误=%v", err)
			}
			message, signatureText, _ := strings.Cut(token, ".")
			payload, err := base64.RawURLEncoding.Strict().DecodeString(message)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"version":1,"snapshot_id":"052dc4f7-f4c1-46b3-a509-bf96992f6580","user_id":"18446744073709551615","device_id":"18446744073709551614","generation":"f10203040506070809101112131415fe","high_water_sequence":"` +
				testCase.want + `","expires_at":"2026-11-23T02:10:00.123456789Z"}`
			if string(payload) != want {
				t.Fatalf("载荷与规范字段不符，实际=%s", payload)
			}
			signature, err := base64.RawURLEncoding.Strict().DecodeString(signatureText)
			if err != nil || len(signature) != sha256.Size {
				t.Fatalf("签名格式错误，错误=%v", err)
			}

			// 2. 独立复算两种用途的 HMAC，续页校验器也须拒绝末页确认凭据。
			checkMAC := func(purpose string) []byte {
				derived := hmac.New(sha256.New, masterKey)
				derived.Write([]byte(purpose))
				mac := hmac.New(sha256.New, derived.Sum(nil))
				mac.Write([]byte(message))
				return mac.Sum(nil)
			}
			if !hmac.Equal(signature, checkMAC("cloudnest/sync-snapshot-baseline/v1")) ||
				hmac.Equal(signature, checkMAC("cloudnest/sync-snapshot-cursor/v1")) {
				t.Fatal("末页确认签名未与续页用途分开")
			}
			verifier, err := NewSyncSnapshotCursorVerifier(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			if verified, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, token, now); verified != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
				t.Fatalf("续页校验器应拒绝末页确认凭据，错误=%v", err)
			}

			// 3. 原绑定重签保持稳定，截止序号变化必须影响凭据。
			replay, err := signer.Sign(binding, testCase.sequence, expiresAt.In(now.Location()))
			if err != nil || replay != token {
				t.Fatalf("相同截止时刻的重签结果不符，错误=%v", err)
			}
			if changed, err := signer.Sign(binding, testCase.sequence^1, expiresAt); err != nil || changed == token {
				t.Fatalf("截止序号变化必须签出不同凭据，错误=%v", err)
			}
			binding.PageSize = 0
			if replay, err := signer.Sign(binding, testCase.sequence, expiresAt); err != nil || replay != token {
				t.Fatalf("确认所需的身份绑定应能独立签发，错误=%v", err)
			}
		})
	}
}

func TestSyncSnapshotBaselineSignerRejectsInvalidInput(t *testing.T) {
	// 1. 主密钥过短或签发器未初始化时拒绝签发。
	if signer, err := NewSyncSnapshotBaselineSigner(make([]byte, sha256.Size-1)); signer != nil || err == nil {
		t.Fatal("应拒绝过短的主密钥")
	}
	binding, _ := syncSnapshotCacheFixture()
	stamp := time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
	var uninitialized SyncSnapshotBaselineSigner
	if token, err := uninitialized.Sign(binding, 0, stamp); token != "" || err == nil {
		t.Fatal("应拒绝未初始化的签发器")
	}
	signer, err := NewSyncSnapshotBaselineSigner(bytes.Repeat([]byte{0x66}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}

	// 2. 异常快照标识、账号、设备、代次和到期时间均产生领域错误。
	for _, testCase := range []struct {
		name   string
		change func(*SyncSnapshotCacheBinding, *time.Time)
	}{
		{"zero snapshot", func(key *SyncSnapshotCacheBinding, _ *time.Time) { key.SnapshotID = uuid.Nil }},
		{"wrong variant", func(key *SyncSnapshotCacheBinding, _ *time.Time) { key.SnapshotID[8] = 0 }},
		{"zero user", func(key *SyncSnapshotCacheBinding, _ *time.Time) { key.UserID = 0 }},
		{"zero device", func(key *SyncSnapshotCacheBinding, _ *time.Time) { key.DeviceID = 0 }},
		{"zero generation", func(key *SyncSnapshotCacheBinding, _ *time.Time) { key.Generation = domain.SyncCursorGeneration{} }},
		{"zero date", func(_ *SyncSnapshotCacheBinding, expires *time.Time) { *expires = time.Time{} }},
		{"UTC year overflow", func(_ *SyncSnapshotCacheBinding, expires *time.Time) {
			*expires = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			key, expires := binding, stamp
			testCase.change(&key, &expires)
			if token, err := signer.Sign(key, 0, expires); token != "" || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
				t.Fatalf("异常快照输入应拒绝签名，错误=%v", err)
			}
		})
	}
}
