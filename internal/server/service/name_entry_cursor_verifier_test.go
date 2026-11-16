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

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// signedNameEntryPayloadForTest 独立为协议边界测试计算正确 HMAC。
func signedNameEntryPayloadForTest(masterKey []byte, encoded string) string {
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/name-entry-cursor/v1"))
	mac := hmac.New(sha256.New, derivation.Sum(nil))
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestNameEntryCursorVerifierRoundTrip(t *testing.T) {
	for _, scope := range []string{"all", "starred"} {
		for _, mode := range []string{"normal", "zero byte", "binary key", "Unicode key", "stored forms", "maximum key", "maximum IDs", "maximum payload", "empty filters", "folder filters", "special filters", "long filters", "long master key", "page size adjustment"} {
			t.Run(scope+"/"+mode, func(t *testing.T) {
				// 1. 签名后还原原始名称键，完整标识和查询原文保留。
				masterKey, query, position := nameEntryCursorSignerFixture()
				query.Scope = scope
				userID := uint64(7)
				switch mode {
				case "zero byte":
					position.NameKey = []byte{0}
				case "binary key":
					position.NameKey = []byte{0, 0xff, 0x80, 'a'}
				case "Unicode key":
					position.NameKey = []byte("学习🌱.md")
				case "stored forms":
					position.NameKey = []byte(" Cafe\u0301.MD ")
				case "maximum key":
					position.NameKey = bytes.Repeat([]byte{0xff}, 768)
				case "maximum IDs":
					userID, position.ID = ^uint64(0), ^uint64(0)
				case "maximum payload":
					userID, position.ID = ^uint64(0), ^uint64(0)
					position.NameKey = bytes.Repeat([]byte{0xff}, 768)
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
				signer, err := NewNameEntryCursorSigner(masterKey)
				if err != nil {
					t.Fatal(err)
				}
				verifier, err := NewNameEntryCursorVerifier(masterKey)
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
				if err != nil || result == nil || !reflect.DeepEqual(*result, position) || query != original || !result.IsValid() {
					t.Fatal("名称游标原始键、标识或查询还原错误", err)
				}
				result.NameKey[0] ^= 1
				if reflect.DeepEqual(*result, position) {
					t.Fatal("名称还原键与来源位置共享字节")
				}
			})
		}
	}
	t.Run("independent Python vector", func(t *testing.T) {
		masterKey, query, want := nameEntryCursorSignerFixture()
		payload := `{"version":1,"user_id":"7","scope":"all","sort":"name_asc","filter_hash":"4a19847aa216599d9b153f3f6ac17a65e8d9e3bbfe8e1c549d56705500b32b22","name_key":"cmVhZG1lLm1k","entry_id":"19"}`
		cursor := base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".P8K6MdPov80N8yXpSwRdLPJ6tmeX4v2dVnPkDraXUQE"
		verifier, err := NewNameEntryCursorVerifier(masterKey)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(7, query, cursor); err != nil || result == nil || !reflect.DeepEqual(*result, want) {
			t.Fatal("名称独立固定向量校验失败", err)
		}
	})
}

func TestNameEntryCursorVerifierSignedPayloadFailures(t *testing.T) {
	scope := domain.GlobalEntryScopeAll
	masterKey, query, position := nameEntryCursorSignerFixture()
	query.Scope = scope
	signer, err := NewNameEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewNameEntryCursorVerifier(masterKey)
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
		{"scope", `"scope":"all"`, `"scope":"starred"`},
		{"sort", `"sort":"name_asc"`, `"sort":"modified_desc"`},
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
		{"missing key", `"name_key":"cmVhZG1lLm1k",`, ``},
		{"null key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":null`},
		{"number key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":123`},
		{"array key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":[]`},
		{"empty key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":""`},
		{"invalid key alphabet", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"private-key!"`},
		{"padded key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"Zg=="`},
		{"key newline", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"cmVh\nZG1lLm1k"`},
		{"noncanonical key bits", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"Zh"`},
		{"standard key alphabet", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"/w"`},
		{"oversized key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"` + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{'a'}, 769)) + `"`},
		{"old time field", `"name_key"`, `"modified_at"`},
		{"unknown field", `{`, `{"extra":true,`},
		{"duplicate field", `{`, `{"user_id":"7",`},
		{"conflicting first field", `{`, `{"user_id":"8",`},
		{"duplicate key", `{`, `{"name_key":"YQ",`},
		{"case changed key", `"user_id"`, `"USER_ID"`},
		{"escaped key name", `"user_id"`, `"\u0075ser_id"`},
		{"escaped entry", `"entry_id":"19"`, `"entry_id":"\u0031\u0039"`},
		{"escaped name key", `"name_key":"cmVhZG1lLm1k"`, `"name_key":"\u0063mVhZG1lLm1k"`},
		{"whitespace", `{`, `{ `},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 即使使用正确 HMAC，错误绑定或非规范载荷也应整体拒绝。
			changed := bytes.Replace(payload, []byte(fixture.old), []byte(fixture.replacement), 1)
			if bytes.Equal(changed, payload) {
				t.Fatal("协议载荷用例未改变数据")
			}
			signed := signedNameEntryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString(changed))
			if result, err := verifier.Verify(7, query, signed); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) || strings.Contains(err.Error(), "private-") {
				t.Fatal("正确 HMAC 的异常载荷被接受或错误暴露输入", err)
			}
		})
	}
	for _, body := range []string{"", `null`, `[]`, `{`, string(payload) + `{}`, `{"user_id":"` + string([]byte{0xff}) + `"}`} {
		signed := signedNameEntryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString([]byte(body)))
		if result, err := verifier.Verify(7, query, signed); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
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
	if result, err := verifier.Verify(7, query, signedNameEntryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString(reordered))); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
		t.Fatal("重排字段未按规范编码拒绝", err)
	}
}

func TestNameEntryCursorVerifierEncodingAndBinding(t *testing.T) {
	scope := domain.GlobalEntryScopeAll
	masterKey, query, position := nameEntryCursorSignerFixture()
	query.Scope = scope
	otherScope := domain.GlobalEntryScopeStarred
	signer, err := NewNameEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewNameEntryCursorVerifier(masterKey)
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
		" " + cursor, cursor + " ", strings.Repeat("A", domain.MaxNameEntryCursorLength+1), strings.Repeat("界", 683),
		signedNameEntryPayloadForTest(masterKey, "%invalid"),
		signedNameEntryPayloadForTest(masterKey, parts[0]+"="),
		signedNameEntryPayloadForTest(masterKey, parts[0][:4]+"\n"+parts[0][4:]),
	} {
		if result, err := verifier.Verify(7, query, invalid); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) || strings.Contains(err.Error(), "private-") {
			t.Fatal("错误分段、编码或签名未统一拒绝", err)
		}
	}
	for _, mode := range []string{"zero user", "other user", "search", "search whitespace", "kind", "scope", "all query", "starred query", "sort", "page size", "large page", "invalid kind", "directory query", "recent query", "invalid UTF-8"} {
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
				input.Sort = domain.GlobalEntrySortModifiedDesc
			case "page size":
				input.PageSize = 0
			case "large page":
				input.PageSize = 101
			case "invalid kind":
				input.Kind = "private-kind"
			case "directory query":
				input.Scope = "directory"
			case "recent query":
				input.Scope, input.Sort = "recent", "last_opened_desc"
			case "invalid UTF-8":
				input.Search = string([]byte{0xff})
			}
			if result, err := verifier.Verify(userID, input, cursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
				t.Fatal("用户或查询条件改变后通过校验", err)
			}
		})
	}
	other, err := NewNameEntryCursorVerifier(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := other.Verify(7, query, cursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
		t.Fatal("其他主密钥通过了校验", err)
	}
	_, _, legacyPosition := globalEntryCursorSignerFixture()
	directorySigner, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	directoryCursor, err := directorySigner.Sign(7, 11, legacyPosition)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := verifier.Verify(7, query, directoryCursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
		t.Fatal("名称校验器接受了目录用途游标", err)
	}
	globalSigner, err := NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{domain.GlobalEntryScopeAll, domain.GlobalEntryScopeStarred} {
		globalQuery := query
		globalQuery.Scope, globalQuery.Sort = scope, domain.GlobalEntrySortModifiedDesc
		globalCursor, err := globalSigner.Sign(7, globalQuery, legacyPosition)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(7, query, globalCursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
			t.Fatal("名称校验器接受全局或收藏用途游标", err)
		}
	}
	recentSigner, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	_, recentQuery, recentPosition := recentEntryCursorSignerFixture()
	recentCursor, err := recentSigner.Sign(7, recentQuery, recentPosition)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := verifier.Verify(7, query, recentCursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
		t.Fatal("名称校验器接受最近用途游标", err)
	}
	for _, length := range []int{0, 1, 31} {
		if component, err := NewNameEntryCursorVerifier(make([]byte, length)); component != nil || err == nil {
			t.Fatal("短主密钥构造成功")
		}
	}
	for _, broken := range []*NameEntryCursorVerifier{nil, {}, {signer: &NameEntryCursorSigner{}}, {signer: &NameEntryCursorSigner{signingKey: []byte{1}}}} {
		if result, err := broken.Verify(7, query, cursor); result != nil || err == nil {
			t.Fatal("未初始化组件通过校验")
		}
	}
}

func TestNameEntryCursorVerifierOwnershipAndConcurrency(t *testing.T) {
	scope := domain.GlobalEntryScopeAll
	masterKey, query, position := nameEntryCursorSignerFixture()
	query.Scope = scope
	signer, err := NewNameEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewNameEntryCursorVerifier(masterKey)
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
	first.NameKey[0] ^= 1
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
				if err != nil || result == nil || !reflect.DeepEqual(*result, position) {
					t.Error("并发校验结果错误", err)
					return
				}
				result.ID = 999
				result.NameKey[0] ^= 1
			}
		})
	}
	workers.Wait()
}
