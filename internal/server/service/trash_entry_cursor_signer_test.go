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

func TestTrashEntryCursorSignerPayload(t *testing.T) {
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	signer, err := NewTrashEntryCursorSigner(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const canonical = "eyJ2ZXJzaW9uIjoxLCJ1c2VyX2lkIjoiNyIsImRlbGV0ZWRfYXQiOiIyMDI2LTExLTIwVDAxOjMwOjAwLjEyMzQ1NloiLCJlbnRyeV9pZCI6IjQyIn0.k0Jcw22xb_sj51k84pGDsVqMZMVg_v-1MhwXc4-Z5oQ"
	const canonicalPayload = `{"version":1,"user_id":"7","deleted_at":"2026-11-20T01:30:00.123456Z","entry_id":"42"}`
	for _, testCase := range []struct {
		name        string
		userID      uint64
		position    domain.TrashEntryPosition
		wantPayload string
		wantCursor  string
	}{
		{"canonical", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp}, canonicalPayload, canonical},
		{"same instant with offset", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp.In(time.FixedZone("UTC+08", 8*60*60))}, canonicalPayload, canonical},
		{"whole second", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp.Truncate(time.Second)},
			`{"version":1,"user_id":"7","deleted_at":"2026-11-20T01:30:00.000000Z","entry_id":"42"}`, ""},
		{"minimum time and identifiers", 1, domain.TrashEntryPosition{ID: 1, DeletedAt: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
			`{"version":1,"user_id":"1","deleted_at":"1000-01-01T00:00:00.000000Z","entry_id":"1"}`, ""},
		{"maximum time and identifiers", math.MaxUint64, domain.TrashEntryPosition{ID: math.MaxUint64, DeletedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
			`{"version":1,"user_id":"18446744073709551615","deleted_at":"9999-12-31T23:59:59.999999Z","entry_id":"18446744073709551615"}`, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 独立核对固定向量、规范分段和完整载荷。
			cursor, err := signer.Sign(testCase.userID, testCase.position)
			if err != nil || (testCase.wantCursor != "" && cursor != testCase.wantCursor) {
				t.Fatalf("游标为 %q，错误为 %v", cursor, err)
			}
			payload, signature, separated := strings.Cut(cursor, ".")
			if !separated || len(cursor) > 208 || strings.ContainsAny(cursor, "=+/ \r\n") {
				t.Fatal("游标分段、长度或 URL 字符错误")
			}
			decoded, err := base64.RawURLEncoding.Strict().DecodeString(payload)
			if err != nil || string(decoded) != testCase.wantPayload {
				t.Fatalf("载荷为 %s，错误为 %v", decoded, err)
			}
			if decoded, err := base64.RawURLEncoding.Strict().DecodeString(signature); err != nil || len(decoded) != sha256.Size {
				t.Fatalf("HMAC 的编码或字节数无效，错误为 %v", err)
			}
		})
	}
}

func TestTrashEntryCursorSignerBinding(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewTrashEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	position := domain.TrashEntryPosition{ID: 42, DeletedAt: time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)}
	sign := func(userID uint64, position domain.TrashEntryPosition) string {
		t.Helper()
		cursor, err := signer.Sign(userID, position)
		if err != nil {
			t.Fatal(err)
		}
		return cursor
	}
	cursor := sign(7, position)
	payload, signature, _ := strings.Cut(cursor, ".")
	// 1. 独立计算其他用途对相同载荷的 HMAC，验证用途隔离。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/recent-entry-cursor/v1"))
	recentMAC := hmac.New(sha256.New, derivation.Sum(nil))
	recentMAC.Write([]byte(payload))
	if base64.RawURLEncoding.EncodeToString(recentMAC.Sum(nil)) == signature {
		t.Fatal("回收站和最近使用共用了签名用途")
	}
	// 2. 变更用户或位置时，签名也随载荷变更。
	for _, changed := range []string{
		sign(8, position),
		sign(7, domain.TrashEntryPosition{ID: 41, DeletedAt: position.DeletedAt}),
		sign(7, domain.TrashEntryPosition{ID: 42, DeletedAt: position.DeletedAt.Add(time.Microsecond)}),
	} {
		_, changedSignature, _ := strings.Cut(changed, ".")
		if changedSignature == signature {
			t.Fatal("签名未绑定用户或续读位置")
		}
	}
	// 3. 调用方修改原主密钥时，已创建的签名器继续使用其派生密钥。
	masterKey[0]++
	if sign(7, position) != cursor {
		t.Fatal("签名器依赖调用方的可变密钥数据")
	}
	differentSigner, err := NewTrashEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := differentSigner.Sign(7, position); err != nil || actual == cursor {
		t.Fatalf("不同主密钥生成了相同签名，错误为 %v", err)
	}
}

func TestTrashEntryCursorSignerInvalidInputs(t *testing.T) {
	for _, size := range []int{0, 31} {
		if signer, err := NewTrashEntryCursorSigner(make([]byte, size)); signer != nil || err == nil {
			t.Fatalf("长度 %d 的主密钥通过，错误为 %v", size, err)
		}
	}
	signer, err := NewTrashEntryCursorSigner(make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	for _, testCase := range []struct {
		name     string
		userID   uint64
		position domain.TrashEntryPosition
	}{
		{"zero user", 0, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp}},
		{"zero entry", 7, domain.TrashEntryPosition{DeletedAt: stamp}},
		{"zero time", 7, domain.TrashEntryPosition{ID: 42}},
		{"excess precision", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp.Add(time.Nanosecond)}},
		{"out of range year", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cursor, err := signer.Sign(testCase.userID, testCase.position)
			if cursor != "" || !errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
				t.Fatalf("无效输入返回了 %q，错误为 %v", cursor, err)
			}
		})
	}
	for _, uninitialized := range []*TrashEntryCursorSigner{nil, {}} {
		cursor, err := uninitialized.Sign(7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp})
		if cursor != "" || err == nil || errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
			t.Fatalf("未初始化签名器返回了 %q，错误为 %v", cursor, err)
		}
	}
}
