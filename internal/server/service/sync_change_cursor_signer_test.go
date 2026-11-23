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

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncChangeCursorSignerCanonicalToken(t *testing.T) {
	// 1. 编码零位置和最大无符号位置，核对真实载荷保留全部代次字节。
	masterKey := bytes.Repeat([]byte{0x98}, sha256.Size)
	signer, err := NewSyncChangeCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name     string
		sequence uint64
		want     string
	}{
		{"zero on empty page", 0, "0"},
		{"maximum unsigned sequence", math.MaxUint64, "18446744073709551615"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			position := domain.SyncChangeCursorPosition{
				UserID: math.MaxUint64, DeviceID: math.MaxUint64 - 1, Sequence: testCase.sequence,
				Generation: domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe},
			}
			token, err := signer.Sign(position)
			if err != nil || len(token) > domain.MaxSyncSnapshotTokenLength || strings.Count(token, ".") != 1 || strings.Contains(token, "=") {
				t.Fatalf("游标文本格式不符：%v", err)
			}
			message, signatureText, _ := strings.Cut(token, ".")
			payload, err := base64.RawURLEncoding.Strict().DecodeString(message)
			want := `{"version":1,"user_id":"18446744073709551615","device_id":"18446744073709551614","generation":"f100030400060708090a0b0c0d0e0ffe","sequence":"` + testCase.want + `"}`
			if err != nil || string(payload) != want {
				t.Fatalf("增量游标载荷不符：%s，错误 %v", payload, err)
			}

			// 2. 独立复算 HMAC，核对增量用途与两种快照凭据隔离。
			signature, err := base64.RawURLEncoding.Strict().DecodeString(signatureText)
			if err != nil || len(signature) != sha256.Size {
				t.Fatalf("签名格式不符：%v", err)
			}
			checkMAC := func(purpose string) []byte {
				derived := hmac.New(sha256.New, masterKey)
				derived.Write([]byte(purpose))
				mac := hmac.New(sha256.New, derived.Sum(nil))
				mac.Write([]byte(message))
				return mac.Sum(nil)
			}
			if !hmac.Equal(signature, checkMAC("cloudnest/sync-change-pull/v1")) ||
				hmac.Equal(signature, checkMAC("cloudnest/sync-snapshot-cursor/v1")) ||
				hmac.Equal(signature, checkMAC("cloudnest/sync-snapshot-baseline/v1")) {
				t.Fatal("游标签名未使用独立的增量拉取用途")
			}

			// 3. 相同输入重签稳定，任一绑定字段变化均产生不同凭据。
			freshSigner, err := NewSyncChangeCursorSigner(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			if repeated, err := freshSigner.Sign(position); err != nil || repeated != token {
				t.Fatalf("同一主密钥和位置重签结果不一致：%v", err)
			}
			for _, change := range []struct {
				name string
				edit func(*domain.SyncChangeCursorPosition)
			}{
				{"user", func(candidate *domain.SyncChangeCursorPosition) { candidate.UserID-- }},
				{"device", func(candidate *domain.SyncChangeCursorPosition) { candidate.DeviceID-- }},
				{"last generation byte", func(candidate *domain.SyncChangeCursorPosition) { candidate.Generation[15] ^= 1 }},
				{"sequence", func(candidate *domain.SyncChangeCursorPosition) { candidate.Sequence ^= 1 }},
			} {
				candidate := position
				change.edit(&candidate)
				if changed, err := signer.Sign(candidate); err != nil || changed == token {
					t.Fatalf("%s 改变后未得到不同游标：%v", change.name, err)
				}
			}
		})
	}
}

func TestSyncChangeCursorSignerRejectsInvalidInput(t *testing.T) {
	// 1. 密钥过短、签发器缺失或未初始化时不交付游标。
	if signer, err := NewSyncChangeCursorSigner(make([]byte, sha256.Size-1)); signer != nil || err == nil {
		t.Fatal("应拒绝过短的主密钥")
	}
	valid := domain.SyncChangeCursorPosition{UserID: 7, DeviceID: 19, Generation: domain.SyncCursorGeneration{15: 1}}
	for _, signer := range []*SyncChangeCursorSigner{nil, {}} {
		if token, err := signer.Sign(valid); token != "" || err == nil {
			t.Fatal("未初始化的签发器仍交付游标")
		}
	}
	signer, err := NewSyncChangeCursorSigner(bytes.Repeat([]byte{0x66}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}

	// 2. 拒绝零账号 ID、零设备 ID 和全零代次，仍允许零位置。
	for _, invalid := range []domain.SyncChangeCursorPosition{
		{DeviceID: valid.DeviceID, Generation: valid.Generation},
		{UserID: valid.UserID, Generation: valid.Generation},
		{UserID: valid.UserID, DeviceID: valid.DeviceID},
	} {
		if token, err := signer.Sign(invalid); token != "" || !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
			t.Fatalf("无效身份或代次未拒绝：%v", err)
		}
	}
	if token, err := signer.Sign(valid); token == "" || err != nil {
		t.Fatalf("代次包含零字节的零位置应能签发：%v", err)
	}
}
