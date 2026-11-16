package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// signedRecentEntryPayloadForTest 独立为协议边界测试计算正确 HMAC。
func signedRecentEntryPayloadForTest(masterKey []byte, encoded string) string {
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/recent-entry-cursor/v1"))
	mac := hmac.New(sha256.New, derivation.Sum(nil))
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestRecentEntryCursorVerifierRoundTrip(t *testing.T) {
	scope := domain.GlobalEntryScopeRecent
	for _, mode := range []string{"normal", "offset", "whole second", "minimum datetime", "maximum datetime", "maximum ids", "empty filters", "folder filters", "special filters", "long filters", "long master key", "page size adjustment"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 将有效查询和数据库位置签名，再还原为独立 UTC 微秒位置。
			masterKey, query, position := recentEntryCursorSignerFixture()
			query.Scope = scope
			userID := uint64(7)
			switch mode {
			case "offset":
				position.LastOpenedAt = position.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))
			case "whole second":
				position.LastOpenedAt = position.LastOpenedAt.Truncate(time.Second)
			case "minimum datetime":
				position.LastOpenedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "maximum datetime":
				position.LastOpenedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "maximum ids":
				userID, position.ID = ^uint64(0), ^uint64(0)
			case "empty filters":
				query.Search, query.Kind = "", ""
			case "folder filters":
				query.Kind = "folder"
			case "special filters":
				query.Search, query.Kind = "<>&\"\\\n\x00\u2028", ""
			case "long filters":
				query.Search = strings.Repeat("界", 2048)
			case "long master key":
				masterKey = bytes.Repeat([]byte{9}, 64)
			}
			signer, err := NewRecentEntryCursorSigner(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := NewRecentEntryCursorVerifier(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			cursor, err := signer.Sign(userID, query, position)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "page size adjustment" {
				query.PageSize = 1
			}
			original := query
			result, err := verifier.Verify(userID, query, cursor)
			if err != nil || result == nil || result.ID != position.ID || !result.LastOpenedAt.Equal(position.LastOpenedAt) || result.LastOpenedAt.Location() != time.UTC || query != original {
				t.Fatal("最近使用游标还原、时区或输入保护错误", err)
			}
		})
	}
	t.Run("independent Python vector", func(t *testing.T) {
		masterKey, query, want := recentEntryCursorSignerFixture()
		query.Scope = scope
		payload := `{"version":1,"user_id":"7","scope":"` + scope + `","sort":"last_opened_desc","filter_hash":"4a19847aa216599d9b153f3f6ac17a65e8d9e3bbfe8e1c549d56705500b32b22","last_opened_at":"2026-11-16T02:30:00.123456Z","entry_id":"19"}`
		expectedSignature := "MGBb-z7AxXj8t5sA5GgTI0RS_iVSgUAYJFbeer6OACc"
		cursor := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + expectedSignature
		verifier, err := NewRecentEntryCursorVerifier(masterKey)
		if err != nil {
			t.Fatal(err)
		}
		if position, err := verifier.Verify(7, query, cursor); err != nil || position == nil || !reflect.DeepEqual(*position, want) {
			t.Fatal("独立固定向量校验失败", err)
		}
	})
}

func TestRecentEntryCursorVerifierSignedPayloadFailures(t *testing.T) {
	scope := domain.GlobalEntryScopeRecent
	masterKey, query, position := recentEntryCursorSignerFixture()
	query.Scope = scope
	otherScope := domain.GlobalEntryScopeAll
	signer, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewRecentEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(cursor, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ name, old, replacement string }{
		{"version", `"version":1`, `"version":2`},
		{"missing version", `"version":1,`, ``},
		{"null version", `"version":1`, `"version":null`},
		{"string version", `"version":1`, `"version":"1"`},
		{"wrong user", `"user_id":"7"`, `"user_id":"8"`},
		{"leading user zero", `"user_id":"7"`, `"user_id":"07"`},
		{"scope", `"scope":"` + scope + `"`, `"scope":"` + otherScope + `"`},
		{"sort", `"sort":"last_opened_desc"`, `"sort":"name_asc"`},
		{"filters", `4a19847aa216599d9b153f3f6ac17a65e8d9e3bbfe8e1c549d56705500b32b22`, strings.Repeat("0", 64)},
		{"missing filters", `"filter_hash":"4a19847aa216599d9b153f3f6ac17a65e8d9e3bbfe8e1c549d56705500b32b22",`, ``},
		{"leading entry zero", `"entry_id":"19"`, `"entry_id":"019"`},
		{"signed entry", `"entry_id":"19"`, `"entry_id":"+19"`},
		{"zero entry", `"entry_id":"19"`, `"entry_id":"0"`},
		{"negative entry", `"entry_id":"19"`, `"entry_id":"-1"`},
		{"overflow entry", `"entry_id":"19"`, `"entry_id":"18446744073709551616"`},
		{"number entry", `"entry_id":"19"`, `"entry_id":19`},
		{"null entry", `"entry_id":"19"`, `"entry_id":null`},
		{"empty entry", `"entry_id":"19"`, `"entry_id":""`},
		{"bad datetime", `2026-11-16T02:30:00.123456Z`, `private-time`},
		{"missing access time", `"last_opened_at":"2026-11-16T02:30:00.123456Z",`, ``},
		{"null access time", `"last_opened_at":"2026-11-16T02:30:00.123456Z"`, `"last_opened_at":null`},
		{"number access time", `"last_opened_at":"2026-11-16T02:30:00.123456Z"`, `"last_opened_at":123`},
		{"modified time field", `"last_opened_at"`, `"modified_at"`},
		{"old datetime", `2026-11-16T02:30:00.123456Z`, `0999-11-16T02:30:00.123456Z`},
		{"far datetime", `2026-11-16T02:30:00.123456Z`, `10000-11-16T02:30:00.123456Z`},
		{"missing fraction", `2026-11-16T02:30:00.123456Z`, `2026-11-16T02:30:00Z`},
		{"milliseconds", `2026-11-16T02:30:00.123456Z`, `2026-11-16T02:30:00.123Z`},
		{"nanoseconds", `2026-11-16T02:30:00.123456Z`, `2026-11-16T02:30:00.123456789Z`},
		{"extra fractional zeros", `2026-11-16T02:30:00.123456Z`, `2026-11-16T02:30:00.123456000Z`},
		{"offset time", `2026-11-16T02:30:00.123456Z`, `2026-11-16T02:30:00.123456+00:00`},
		{"unknown field", `{`, `{"extra":true,`},
		{"duplicate field", `{`, `{"user_id":"7",`},
		{"conflicting first field", `{`, `{"user_id":"8",`},
		{"case changed key", `"user_id"`, `"USER_ID"`},
		{"escaped key", `"user_id"`, `"\u0075ser_id"`},
		{"escaped entry", `"entry_id":"19"`, `"entry_id":"\u0031\u0039"`},
		{"whitespace", `{`, `{ `},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 即使使用正确 HMAC，错误绑定或非规范载荷也应整体拒绝。
			changed := bytes.Replace(payload, []byte(fixture.old), []byte(fixture.replacement), 1)
			if bytes.Equal(changed, payload) {
				t.Fatal("协议载荷用例未改变数据")
			}
			signed := signedRecentEntryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString(changed))
			if result, err := verifier.Verify(7, query, signed); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) || strings.Contains(err.Error(), "private-") {
				t.Fatal("正确 HMAC 的异常载荷被接受或错误暴露输入", err)
			}
		})
	}
	for _, body := range []string{"", `null`, `[]`, `{`, string(payload) + `{}`, `{"user_id":"` + string([]byte{0xff}) + `"}`} {
		signed := signedRecentEntryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString([]byte(body)))
		if result, err := verifier.Verify(7, query, signed); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
			t.Fatal("无效 JSON 载荷被接受", err)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	reordered, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := verifier.Verify(7, query, signedRecentEntryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString(reordered))); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
		t.Fatal("重排字段未按规范编码拒绝", err)
	}
}

func TestRecentEntryCursorVerifierEncodingAndBinding(t *testing.T) {
	scope := domain.GlobalEntryScopeRecent
	masterKey, query, position := recentEntryCursorSignerFixture()
	query.Scope = scope
	otherScope := domain.GlobalEntryScopeAll
	signer, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewRecentEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(cursor, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	changedSignature := bytes.Clone(signature)
	changedSignature[0] ^= 1
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	lastIndex := strings.IndexByte(alphabet, parts[1][len(parts[1])-1])
	noncanonical := parts[1][:len(parts[1])-1] + string(alphabet[lastIndex|1])
	for _, invalid := range []string{
		"", "private-cursor", parts[0], cursor + ".extra", "." + parts[1], parts[0] + ".",
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(changedSignature),
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(signature[:31]),
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(append(bytes.Clone(signature), 0)),
		parts[0] + "." + parts[1] + "=", parts[0] + "." + parts[1] + "\n", parts[0] + "." + noncanonical,
		" " + cursor, cursor + " ", strings.Repeat("A", domain.MaxRecentEntryCursorLength+1),
		signedRecentEntryPayloadForTest(masterKey, "%invalid"),
		signedRecentEntryPayloadForTest(masterKey, parts[0]+"="),
		signedRecentEntryPayloadForTest(masterKey, parts[0][:4]+"\n"+parts[0][4:]),
	} {
		if result, err := verifier.Verify(7, query, invalid); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) || strings.Contains(err.Error(), "private-") {
			t.Fatal("错误分段、编码或签名未统一拒绝", err)
		}
	}
	for _, mode := range []string{"zero user", "other user", "search", "search whitespace", "kind", "scope", "all query", "starred query", "sort", "page size", "invalid UTF-8"} {
		t.Run(mode, func(t *testing.T) {
			userID, input := uint64(7), query
			switch mode {
			case "zero user":
				userID = 0
			case "other user":
				userID = 8
			case "search":
				input.Search = "other"
			case "search whitespace":
				input.Search += " "
			case "kind":
				input.Kind = "image"
			case "scope":
				input.Scope = otherScope
			case "all query", "starred query":
				input.Scope, input.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortModifiedDesc
				if mode == "starred query" {
					input.Scope = domain.GlobalEntryScopeStarred
				}
			case "sort":
				input.Sort = domain.GlobalEntrySortNameAsc
			case "page size":
				input.PageSize = 0
			case "invalid UTF-8":
				input.Search = string([]byte{0xff})
			}
			if result, err := verifier.Verify(userID, input, cursor); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
				t.Fatal("用户或查询条件改变后通过校验", err)
			}
		})
	}
	other, err := NewRecentEntryCursorVerifier(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := other.Verify(7, query, cursor); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
		t.Fatal("其他主密钥通过了校验", err)
	}
	directorySigner, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	directoryCursor, err := directorySigner.Sign(7, 11, domain.FileEntryPosition{ModifiedAt: position.LastOpenedAt, ID: position.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := verifier.Verify(7, query, directoryCursor); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
		t.Fatal("最近使用校验器接受了目录用途游标", err)
	}
	globalSigner, err := NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{domain.GlobalEntryScopeAll, domain.GlobalEntryScopeStarred} {
		globalQuery := query
		globalQuery.Scope, globalQuery.Sort = scope, domain.GlobalEntrySortModifiedDesc
		globalCursor, err := globalSigner.Sign(7, globalQuery, domain.FileEntryPosition{ModifiedAt: position.LastOpenedAt, ID: position.ID})
		if err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(7, query, globalCursor); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
			t.Fatal("最近使用校验器接受全局或收藏用途游标", err)
		}
	}
	for _, length := range []int{0, 1, 31} {
		if component, err := NewRecentEntryCursorVerifier(make([]byte, length)); component != nil || err == nil {
			t.Fatal("短主密钥构造成功")
		}
	}
	for _, broken := range []*RecentEntryCursorVerifier{nil, {}, {signer: &RecentEntryCursorSigner{}}, {signer: &RecentEntryCursorSigner{signingKey: []byte{1}}}} {
		if result, err := broken.Verify(7, query, cursor); result != nil || err == nil {
			t.Fatal("未初始化组件通过校验")
		}
	}
}

func TestRecentEntryCursorVerifierOwnershipAndConcurrency(t *testing.T) {
	scope := domain.GlobalEntryScopeRecent
	masterKey, query, position := recentEntryCursorSignerFixture()
	query.Scope = scope
	signer, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewRecentEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	for index := range masterKey {
		masterKey[index] = 0
	}
	first, err := verifier.Verify(7, query, cursor)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	first.ID = 999
	first.LastOpenedAt = time.Time{}
	second, err := verifier.Verify(7, query, cursor)
	if err != nil || second == nil || !reflect.DeepEqual(*second, position) || first == second {
		t.Fatal("密钥缓冲区或前次结果影响校验", err)
	}
	encoded, err := json.Marshal(verifier)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("组件序列化暴露内部数据", err)
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				result, err := verifier.Verify(7, query, cursor)
				if err != nil || result == nil || *result != position {
					t.Error("并发校验结果错误", err)
					return
				}
				result.ID = 999
			}
		})
	}
	workers.Wait()
}
