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

func TestTrashEntryCursorVerifierPositions(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewTrashEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewTrashEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	masterKey[0]++
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	// 1. 验证独立测试向量及签发器输出，检查 UTC 与完整无符号 ID。
	for _, testCase := range []struct {
		name     string
		userID   uint64
		position domain.TrashEntryPosition
	}{
		{"fixed vector", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp}},
		{"time zone offset", 7, domain.TrashEntryPosition{ID: 42, DeletedAt: stamp.In(time.FixedZone("UTC+08", 8*60*60))}},
		{"minimum values", 1, domain.TrashEntryPosition{ID: 1, DeletedAt: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{"maximum values", math.MaxUint64, domain.TrashEntryPosition{ID: math.MaxUint64, DeletedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cursor := trashCursorFixture
			if testCase.name != "fixed vector" {
				var err error
				cursor, err = signer.Sign(testCase.userID, testCase.position)
				if err != nil {
					t.Fatal(err)
				}
			}
			want := domain.TrashEntryPosition{ID: testCase.position.ID, DeletedAt: testCase.position.DeletedAt.UTC()}
			position, err := verifier.Verify(testCase.userID, cursor)
			if err != nil || position == nil || *position != want {
				t.Fatalf("还原位置为 %+v，期望 %+v，错误为 %v", position, want, err)
			}
		})
	}
}

func TestTrashEntryCursorVerifierRejectsInvalidText(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	verifier, err := NewTrashEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, signature, _ := strings.Cut(trashCursorFixture, ".")
	// 1. 拒绝错误用户、错误分段、非规范文本和改动过的签名。
	for _, testCase := range []struct {
		name   string
		userID uint64
		cursor string
	}{
		{"zero user", 0, trashCursorFixture},
		{"different user", 8, trashCursorFixture},
		{"empty", 7, ""},
		{"too long", 7, strings.Repeat("a", 1025)},
		{"missing separator", 7, payload},
		{"extra segment", 7, trashCursorFixture + ".more"},
		{"empty payload", 7, "." + signature},
		{"short signature", 7, payload + "." + signature[1:]},
		{"signature padding", 7, trashCursorFixture + "="},
		{"payload changed", 7, "A" + trashCursorFixture[1:]},
		{"signature changed", 7, payload + ".A" + signature[1:]},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			position, err := verifier.Verify(testCase.userID, testCase.cursor)
			if position != nil || !errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
				t.Fatalf("无效游标返回位置 %+v，错误为 %v", position, err)
			}
		})
	}
	recentSigner, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	query := domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeRecent, Sort: domain.GlobalEntrySortLastOpenedDesc, PageSize: 50}
	recentCursor, err := recentSigner.Sign(7, query, domain.RecentEntryPosition{ID: 42, LastOpenedAt: time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if position, err := verifier.Verify(7, recentCursor); position != nil || !errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
		t.Fatalf("其他用途游标返回位置 %+v，错误为 %v", position, err)
	}
}

func TestTrashEntryCursorVerifierRejectsNoncanonicalPayload(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	verifier, err := NewTrashEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	const payload = `{"version":1,"user_id":"7","deleted_at":"2026-11-20T01:30:00.123456Z","entry_id":"42"}`
	// 1. 用独立测试密钥给异常载荷生成签名，单独验证字段与规范编码。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/trash-entry-cursor/v1"))
	for _, testCase := range []struct {
		name    string
		current string
		replace string
	}{
		{"unsupported version", `"version":1`, `"version":2`},
		{"leading zero user", `"user_id":"7"`, `"user_id":"07"`},
		{"numeric entry ID", `"entry_id":"42"`, `"entry_id":42`},
		{"missing entry ID", `,"entry_id":"42"`, ""},
		{"overflow entry ID", `"entry_id":"42"`, `"entry_id":"18446744073709551616"`},
		{"zero entry ID", `"entry_id":"42"`, `"entry_id":"0"`},
		{"leading zero entry ID", `"entry_id":"42"`, `"entry_id":"042"`},
		{"duplicate ID", `"entry_id":"42"`, `"entry_id":"42","entry_id":"42"`},
		{"unknown key", `"entry_id":"42"`, `"entry_id":"42","extra":true`},
		{"offset time", ".123456Z", ".123456+00:00"},
		{"comma in time", ".123456Z", ",123456Z"},
		{"extra precision", ".123456Z", ".123456000Z"},
		{"year outside MySQL range", "2026-", "0999-"},
		{"trailing whitespace", "}", "} "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			replaced := strings.Replace(payload, testCase.current, testCase.replace, 1)
			encoded := base64.RawURLEncoding.EncodeToString([]byte(replaced))
			mac := hmac.New(sha256.New, derivation.Sum(nil))
			mac.Write([]byte(encoded))
			cursor := encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			position, err := verifier.Verify(7, cursor)
			if replaced == payload || position != nil || !errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
				t.Fatalf("非规范载荷返回位置 %+v，错误为 %v", position, err)
			}
		})
	}
}

func TestTrashEntryCursorVerifierConfiguration(t *testing.T) {
	if verifier, err := NewTrashEntryCursorVerifier(make([]byte, 31)); verifier != nil || err == nil {
		t.Fatalf("过短主密钥返回校验器 %+v，错误为 %v", verifier, err)
	}
	for _, verifier := range []*TrashEntryCursorVerifier{nil, {}} {
		position, err := verifier.Verify(7, trashCursorFixture)
		if position != nil || err == nil || errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
			t.Fatalf("校验器配置错误返回位置 %+v，错误为 %v", position, err)
		}
	}
}
