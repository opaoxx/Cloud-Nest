package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// signedDirectoryPayloadForTest 为测试中的任意编码载荷生成正确 HMAC，覆盖签名后的协议校验。
func signedDirectoryPayloadForTest(masterKey []byte, encodedPayload string) string {
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/directory-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(encodedPayload))
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
}

func TestDirectoryCursorVerifierRoundTrip(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	for _, testCase := range []struct {
		name                      string
		userID, parentID, entryID uint64
		stamp                     time.Time
	}{
		{"microseconds", 7, 11, 19, stamp},
		{"offset source", 7, 11, 19, stamp.In(time.FixedZone("fixture", 8*3600))},
		{"whole second", 7, 11, 19, stamp.Truncate(time.Second)},
		{"minimum datetime", 1, 2, 3, time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"maximum datetime", 1, 2, 3, time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
		{"maximum ids", ^uint64(0), ^uint64(0) - 1, ^uint64(0), stamp},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cursor, err := signer.Sign(testCase.userID, testCase.parentID, domain.FileEntryPosition{ModifiedAt: testCase.stamp, ID: testCase.entryID})
			if err != nil {
				t.Fatal(err)
			}
			position, err := verifier.Verify(testCase.userID, testCase.parentID, cursor)
			if err != nil || position == nil || position.ID != testCase.entryID || !position.ModifiedAt.Equal(testCase.stamp) || position.ModifiedAt.Location() != time.UTC || len(cursor) > domain.MaxDirectoryCursorLength {
				t.Fatal("签名与校验还原错误", err)
			}
		})
	}
	t.Run("independent Python vector", func(t *testing.T) {
		key := make([]byte, 32)
		for index := range key {
			key[index] = byte(index)
		}
		fixtureVerifier, err := NewDirectoryCursorVerifier(key)
		if err != nil {
			t.Fatal(err)
		}
		filterHash := sha256.Sum256([]byte(`{"q":"","kind":""}`))
		body := `{"version":1,"user_id":"7","parent_id":"11","scope":"directory","sort":"modified_desc","filter_hash":"` + hex.EncodeToString(filterHash[:]) + `","modified_at":"2026-11-15T02:30:00.123456Z","entry_id":"19"}`
		cursor := base64.RawURLEncoding.EncodeToString([]byte(body)) + ".SdQvO_s7JxgV-UUnKn2dp7c_a9HDuuCb2hksSCXSIhA"
		if position, err := fixtureVerifier.Verify(7, 11, cursor); err != nil || position == nil || position.ID != 19 || !position.ModifiedAt.Equal(stamp) {
			t.Fatal("独立向量校验失败", err)
		}
	})
}

func TestDirectoryCursorVerifierSignedPayloadFailures(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := signer.Sign(7, 11, domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC), ID: 19})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(cursor, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	filterHash := sha256.Sum256([]byte(`{"q":"","kind":""}`))
	for _, testCase := range []struct{ name, old, replacement string }{
		{"version", `"version":1`, `"version":2`},
		{"missing version", `"version":1,`, ``},
		{"null version", `"version":1`, `"version":null`},
		{"string version", `"version":1`, `"version":"1"`},
		{"wrong user", `"user_id":"7"`, `"user_id":"8"`},
		{"wrong parent", `"parent_id":"11"`, `"parent_id":"12"`},
		{"scope", `"scope":"directory"`, `"scope":"all"`},
		{"sort", `"sort":"modified_desc"`, `"sort":"name_asc"`},
		{"filters", hex.EncodeToString(filterHash[:]), strings.Repeat("0", 64)},
		{"leading user zero", `"user_id":"7"`, `"user_id":"07"`},
		{"leading parent zero", `"parent_id":"11"`, `"parent_id":"011"`},
		{"leading entry zero", `"entry_id":"19"`, `"entry_id":"019"`},
		{"zero entry", `"entry_id":"19"`, `"entry_id":"0"`},
		{"negative entry", `"entry_id":"19"`, `"entry_id":"-1"`},
		{"overflow entry", `"entry_id":"19"`, `"entry_id":"18446744073709551616"`},
		{"number entry", `"entry_id":"19"`, `"entry_id":19`},
		{"null entry", `"entry_id":"19"`, `"entry_id":null`},
		{"empty entry", `"entry_id":"19"`, `"entry_id":""`},
		{"bad datetime", `2026-11-15T02:30:00.123456Z`, `invalid-time`},
		{"old datetime", `2026-11-15T02:30:00.123456Z`, `0999-11-15T02:30:00.123456Z`},
		{"far datetime", `2026-11-15T02:30:00.123456Z`, `10000-11-15T02:30:00.123456Z`},
		{"missing fraction", `2026-11-15T02:30:00.123456Z`, `2026-11-15T02:30:00Z`},
		{"milliseconds", `2026-11-15T02:30:00.123456Z`, `2026-11-15T02:30:00.123Z`},
		{"nanoseconds", `2026-11-15T02:30:00.123456Z`, `2026-11-15T02:30:00.123456789Z`},
		{"extra fractional zeros", `2026-11-15T02:30:00.123456Z`, `2026-11-15T02:30:00.123456000Z`},
		{"offset datetime", `2026-11-15T02:30:00.123456Z`, `2026-11-15T02:30:00.123456+00:00`},
		{"unknown field", `{`, `{"extra":true,`},
		{"duplicate field", `{`, `{"user_id":"7",`},
		{"duplicate conflicting first field", `{`, `{"user_id":"8",`},
		{"case changed key", `"user_id"`, `"USER_ID"`},
		{"escaped key", `"user_id"`, `"\u0075ser_id"`},
		{"escaped id", `"entry_id":"19"`, `"entry_id":"\u0031\u0039"`},
		{"whitespace", `{`, `{ `},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := bytes.Replace(payload, []byte(testCase.old), []byte(testCase.replacement), 1)
			if bytes.Equal(changed, payload) {
				t.Fatal("签名载荷用例未改变数据")
			}
			signed := signedDirectoryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString(changed))
			if result, err := verifier.Verify(7, 11, signed); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
				t.Fatal("正确 HMAC 的无效协议载荷被接受", err)
			}
		})
	}
	for _, body := range []string{"", `null`, `[]`, `{`, string(payload) + `{}`, `{"user_id":"` + string([]byte{0xff}) + `"}`} {
		signed := signedDirectoryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString([]byte(body)))
		if result, err := verifier.Verify(7, 11, signed); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
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
	if result, err := verifier.Verify(7, 11, signedDirectoryPayloadForTest(masterKey, base64.RawURLEncoding.EncodeToString(reordered))); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
		t.Fatal("字段重排未按规范协议拒绝", err)
	}
}

func TestDirectoryCursorVerifierEncodingAndBinding(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := signer.Sign(7, 11, domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC), ID: 19})
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
	noncanonicalSignature := parts[1][:len(parts[1])-1] + string(alphabet[lastIndex|1])
	for _, invalid := range []string{
		"", "private-cursor", parts[0], cursor + ".extra", "." + parts[1], parts[0] + ".",
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(changedSignature),
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(signature[:31]),
		parts[0] + "." + base64.RawURLEncoding.EncodeToString(append(bytes.Clone(signature), byte(0))),
		parts[0] + "." + parts[1] + "=", parts[0] + "." + parts[1] + "\n", parts[0] + "." + noncanonicalSignature,
		" " + cursor, cursor + " ", strings.Repeat("A", domain.MaxDirectoryCursorLength+1),
		signedDirectoryPayloadForTest(masterKey, "%invalid"),
		signedDirectoryPayloadForTest(masterKey, parts[0]+"="),
		signedDirectoryPayloadForTest(masterKey, parts[0][:4]+"\n"+parts[0][4:]),
	} {
		result, err := verifier.Verify(7, 11, invalid)
		if result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) || strings.Contains(err.Error(), "private-cursor") {
			t.Fatal("错误编码未统一拒绝或包含输入原文", err)
		}
	}
	for _, scope := range [][2]uint64{{0, 11}, {7, 0}, {8, 11}, {7, 12}} {
		if result, err := verifier.Verify(scope[0], scope[1], cursor); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
			t.Fatal("当前用户或父目录绑定错误", err)
		}
	}
	other, err := NewDirectoryCursorVerifier(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := other.Verify(7, 11, cursor); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
		t.Fatal("其他密钥接受了游标", err)
	}
	for _, length := range []int{0, 1, 31} {
		if verifier, err := NewDirectoryCursorVerifier(make([]byte, length)); verifier != nil || err == nil {
			t.Fatal("错误主密钥仍构造成功")
		}
	}
	for _, invalid := range []*DirectoryCursorVerifier{nil, {}, {signer: &DirectoryCursorSigner{}}, {signer: &DirectoryCursorSigner{signingKey: []byte{1}}}} {
		if result, err := invalid.Verify(7, 11, cursor); result != nil || err == nil {
			t.Fatal("未初始化组件仍通过校验")
		}
	}
}

func TestDirectoryCursorVerifierOwnershipAndConcurrency(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC), ID: 19}
	cursor, err := signer.Sign(7, 11, want)
	if err != nil {
		t.Fatal(err)
	}
	for index := range masterKey {
		masterKey[index] = 0
	}
	first, err := verifier.Verify(7, 11, cursor)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	first.ID = 99
	second, err := verifier.Verify(7, 11, cursor)
	if err != nil || second == nil || !reflect.DeepEqual(*second, want) || first == second {
		t.Fatal("来源密钥或前次结果影响校验", err)
	}
	encoded, err := json.Marshal(verifier)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("校验器序列化包含密钥", err)
	}
	type result struct {
		position *domain.FileEntryPosition
		err      error
	}
	results := make(chan result, 64)
	for range 64 {
		go func() {
			position, err := verifier.Verify(7, 11, cursor)
			results <- result{position: position, err: err}
		}()
	}
	for range 64 {
		current := <-results
		if current.err != nil || current.position == nil || !reflect.DeepEqual(*current.position, want) {
			t.Fatal("并发校验结果异常", current.err)
		}
	}
}
