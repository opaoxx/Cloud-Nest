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

func syncSnapshotBaselineVerificationFixture(t *testing.T) (*SyncSnapshotBaselineVerifier, SyncSnapshotCacheBinding, time.Time, []byte) {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x65}, sha256.Size)
	verifier, err := NewSyncSnapshotBaselineVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := syncSnapshotCacheFixture()
	binding.UserID, binding.DeviceID = math.MaxUint64, math.MaxUint64-1
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 123456789, time.UTC)
	return verifier, binding, expiresAt, masterKey
}

func TestSyncSnapshotBaselineVerifierRoundTripAndExpiry(t *testing.T) {
	// 1. 还原零值和最大序号、完整设备代次以及纳秒级期限。
	verifier, binding, expiresAt, _ := syncSnapshotBaselineVerificationFixture(t)
	for _, sequence := range []uint64{0, math.MaxUint64} {
		token, err := verifier.signer.Sign(binding, sequence, expiresAt)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			result, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, token, expiresAt.Add(-time.Nanosecond))
			want := VerifiedSyncSnapshotBaseline{
				SnapshotID: binding.SnapshotID, UserID: binding.UserID, DeviceID: binding.DeviceID, Generation: binding.Generation,
				HighWaterSequence: sequence, ExpiresAt: expiresAt,
			}
			if err != nil || result == nil || *result != want {
				t.Fatalf("完整确认信息未还原，结果=%+v，错误=%v", result, err)
			}
		}

		// 2. 凭据在等于截止时间时即失效，后续重复校验得到同类错误。
		for _, now := range []time.Time{expiresAt, expiresAt.Add(time.Nanosecond)} {
			if result, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, token, now); result != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
				t.Fatalf("达到到期时间应返回失效，错误=%v", err)
			}
		}
	}
}

func TestSyncSnapshotBaselineVerifierForPull(t *testing.T) {
	// 1. 首次拉取从已验签载荷取得快照 ID，两个入口还原相同的可信信息。
	verifier, binding, expiresAt, _ := syncSnapshotBaselineVerificationFixture(t)
	beforeExpiry := expiresAt.Add(-time.Nanosecond)
	for _, sequence := range []uint64{0, math.MaxUint64} {
		token, err := verifier.signer.Sign(binding, sequence, expiresAt)
		if err != nil {
			t.Fatal(err)
		}
		result, err := verifier.VerifyForPull(binding.UserID, binding.DeviceID, token, beforeExpiry)
		want := VerifiedSyncSnapshotBaseline{
			SnapshotID: binding.SnapshotID, UserID: binding.UserID, DeviceID: binding.DeviceID,
			Generation: binding.Generation, HighWaterSequence: sequence, ExpiresAt: expiresAt,
		}
		if err != nil || result == nil || *result != want {
			t.Fatalf("未从末页凭据还原快照或续读信息：%+v，错误 %v", result, err)
		}
		confirmation, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, token, beforeExpiry)
		if err != nil || confirmation == nil || *confirmation != *result {
			t.Fatalf("确认和首次拉取验签结果不一致：%+v，错误 %v", confirmation, err)
		}

		// 2. 原期限含等号。确认路径不符仍先视为无效凭据。
		for _, now := range []time.Time{expiresAt, expiresAt.Add(time.Nanosecond)} {
			if expired, err := verifier.VerifyForPull(binding.UserID, binding.DeviceID, token, now); expired != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
				t.Fatalf("首次拉取未按原期限拒绝过期凭据：%v", err)
			}
		}
		wrongID := binding.SnapshotID
		wrongID[15] ^= 1
		for _, now := range []time.Time{beforeExpiry, expiresAt} {
			if invalid, err := verifier.Verify(binding.UserID, binding.DeviceID, wrongID, token, now); invalid != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
				t.Fatalf("确认路径未匹配却交付了结果：%v", err)
			}
		}
	}
}

func TestSyncSnapshotBaselineVerifierRejectsWrongBindingAndSignature(t *testing.T) {
	// 1. 账号、设备和快照标识不符时按无效处理，包括凭据已到期时。
	verifier, binding, expiresAt, masterKey := syncSnapshotBaselineVerificationFixture(t)
	token, err := verifier.signer.Sign(binding, 20, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	otherSnapshot := binding
	otherSnapshot.SnapshotID[15] ^= 1
	for _, scope := range []SyncSnapshotCacheBinding{
		{UserID: binding.UserID - 1, DeviceID: binding.DeviceID, SnapshotID: binding.SnapshotID},
		{UserID: binding.UserID, DeviceID: binding.DeviceID - 1, SnapshotID: binding.SnapshotID},
		otherSnapshot,
	} {
		if result, err := verifier.Verify(scope.UserID, scope.DeviceID, scope.SnapshotID, token, expiresAt); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
			t.Fatalf("绑定不符应返回无效，错误=%v", err)
		}
		if scope.UserID != binding.UserID || scope.DeviceID != binding.DeviceID {
			if result, err := verifier.VerifyForPull(scope.UserID, scope.DeviceID, token, expiresAt); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
				t.Fatalf("首次拉取使用其他账号或设备通过验签：%v", err)
			}
		}
	}

	// 2. 拒绝续页用途、错误签名及不符合凭据外层格式的输入。
	cursorSigner, err := NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := cursorSigner.Sign(binding, 50, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := NewSyncSnapshotBaselineVerifier(bytes.Repeat([]byte{0x66}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := wrongKey.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, token, expiresAt); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
		t.Fatalf("错误密钥应返回无效，错误=%v", err)
	}
	if result, err := wrongKey.VerifyForPull(binding.UserID, binding.DeviceID, token, expiresAt); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
		t.Fatalf("首次拉取使用错误密钥通过验签：%v", err)
	}
	changeSigner, err := NewSyncChangeCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	changeCursor, err := changeSigner.Sign(domain.SyncChangeCursorPosition{
		UserID: binding.UserID, DeviceID: binding.DeviceID, Generation: binding.Generation, Sequence: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, signature, _ := strings.Cut(token, ".")
	for _, candidate := range []string{"", " ", strings.Repeat("a", domain.MaxSyncSnapshotTokenLength+1), token + ".extra",
		message + "=." + signature, message + "." + signature + "=", message + "A." + signature, cursor, changeCursor} {
		if result, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, candidate, expiresAt); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
			t.Fatalf("异常凭据应返回无效，错误=%v", err)
		}
		if result, err := verifier.VerifyForPull(binding.UserID, binding.DeviceID, candidate, expiresAt.Add(-time.Nanosecond)); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
			t.Fatalf("首次拉取接受了无效或错用用途的凭据：%v", err)
		}
	}
}

func TestSyncSnapshotBaselineVerifierRejectsSignedNoncanonicalPayload(t *testing.T) {
	// 1. 用测试专用密钥构造独立的异常载荷，确认验签成功后仍检查内容格式。
	verifier, binding, expiresAt, _ := syncSnapshotBaselineVerificationFixture(t)
	token, err := verifier.signer.Sign(binding, 0, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, _ := strings.Cut(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := binding.SnapshotID.String()
	for _, testCase := range []struct{ name, old, replacement string }{
		{"version", `"version":1`, `"version":2`},
		{"duplicate field", `"version":1`, `"version":1,"version":1`},
		{"unknown field", `"version":1`, `"version":1,"extra":0`},
		{"missing snapshot id", `"snapshot_id":"` + snapshotID + `",`, ``},
		{"malformed snapshot id", snapshotID, `not-a-uuid`},
		{"zero snapshot id", snapshotID, `00000000-0000-0000-0000-000000000000`},
		{"non-v4 snapshot id", snapshotID, `052dc4f7-f4c1-16b3-a509-bf96992f6580`},
		{"wrong snapshot variant", snapshotID, `052dc4f7-f4c1-46b3-0509-bf96992f6580`},
		{"uppercase snapshot id", snapshotID, strings.ToUpper(snapshotID)},
		{"leading zero sequence", `"high_water_sequence":"0"`, `"high_water_sequence":"00"`},
		{"sequence exceeds uint64", `"high_water_sequence":"0"`, `"high_water_sequence":"18446744073709551616"`},
		{"sequence JSON number", `"high_water_sequence":"0"`, `"high_water_sequence":0`},
		{"missing sequence", `"high_water_sequence":"0",`, ``},
		{"zero generation", `f10203040506070809101112131415fe`, `00000000000000000000000000000000`},
		{"uppercase generation", `f10203040506070809101112131415fe`, `F10203040506070809101112131415FE`},
		{"short generation", `f10203040506070809101112131415fe`, `f102030405060708`},
		{"offset timestamp", `2026-11-23T02:10:00.123456789Z`, `2026-11-23T10:10:00.123456789+08:00`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := strings.Replace(string(payload), testCase.old, testCase.replacement, 1)
			if changed == string(payload) {
				t.Fatal("测试载荷未修改")
			}
			message := base64.RawURLEncoding.EncodeToString([]byte(changed))
			candidate := message + "." + base64.RawURLEncoding.EncodeToString(syncSnapshotTokenMAC(verifier.signer.signingKey, message))
			if result, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, candidate, expiresAt.Add(-time.Nanosecond)); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
				t.Fatalf("非规范凭据应拒绝，错误=%v", err)
			}
			if result, err := verifier.VerifyForPull(binding.UserID, binding.DeviceID, candidate, expiresAt.Add(-time.Nanosecond)); result != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
				t.Fatalf("首次拉取接受了已签名但非规范的凭据：%v", err)
			}
		})
	}
}
