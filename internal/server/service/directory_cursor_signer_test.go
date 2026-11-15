package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// readDirectoryCursorForTest 独立解析格式，并用标准库从测试主密钥重算 HMAC。
func readDirectoryCursorForTest(t *testing.T, cursor string, masterKey []byte) map[string]any {
	t.Helper()
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || strings.ContainsAny(cursor, "=+/ \r\n") {
		t.Fatal("游标分段或 URL 安全文本格式错误")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		t.Fatal("游标载荷编码不规范", err)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		t.Fatal("游标签名编码错误", err)
	}
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/directory-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0]))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("游标 HMAC 与独立计算不一致")
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestDirectoryCursorSignerPayload(t *testing.T) {
	masterKey := make([]byte, 32)
	for index := range masterKey {
		masterKey[index] = byte(index)
	}
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	for _, testCase := range []struct {
		name                      string
		userID, parentID, entryID uint64
		stamp                     time.Time
	}{
		{name: "canonical fixture", userID: 7, parentID: 11, entryID: 19, stamp: stamp},
		{name: "offset time", userID: 7, parentID: 11, entryID: 19, stamp: stamp.In(time.FixedZone("fixture", 8*3600))},
		{name: "whole second", userID: 7, parentID: 11, entryID: 19, stamp: stamp.Truncate(time.Second)},
		{name: "minimum datetime", userID: 1, parentID: 2, entryID: 3, stamp: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "maximum datetime", userID: 1, parentID: 2, entryID: 3, stamp: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
		{name: "maximum unsigned ids", userID: ^uint64(0), parentID: ^uint64(0) - 1, entryID: ^uint64(0), stamp: stamp},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 生成游标，独立解码并核对八个字段及完整整数。
			position := domain.FileEntryPosition{ModifiedAt: testCase.stamp, ID: testCase.entryID}
			original := position
			cursor, err := signer.Sign(testCase.userID, testCase.parentID, position)
			if err != nil || cursor == "" {
				t.Fatal(err)
			}
			fields := readDirectoryCursorForTest(t, cursor, masterKey)
			filterHash := sha256.Sum256([]byte(`{"q":"","kind":""}`))
			if len(fields) != 8 || fields["version"] != float64(1) || fields["user_id"] != strconv.FormatUint(testCase.userID, 10) || fields["parent_id"] != strconv.FormatUint(testCase.parentID, 10) || fields["entry_id"] != strconv.FormatUint(testCase.entryID, 10) || fields["scope"] != "directory" || fields["sort"] != "modified_desc" || fields["filter_hash"] != hex.EncodeToString(filterHash[:]) || fields["modified_at"] != testCase.stamp.UTC().Format("2006-01-02T15:04:05.000000Z") {
				t.Fatal("游标字段、范围或微秒精度错误")
			}
			if !reflect.DeepEqual(position, original) {
				t.Fatal("签名修改了来源位置")
			}

			// 2. 使用独立 Python 标准库生成的固定向量核对格式与签名。
			if testCase.name == "canonical fixture" || testCase.name == "offset time" {
				if signature := strings.Split(cursor, ".")[1]; signature != "SdQvO_s7JxgV-UUnKn2dp7c_a9HDuuCb2hksSCXSIhA" {
					t.Fatal("签名与独立固定向量不一致")
				}
			}
		})
	}
}

func TestDirectoryCursorSignerTampering(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	position := domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC), ID: 19}
	cursor, err := signer.Sign(7, 11, position)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(cursor, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/directory-cursor/v1"))
	key := derivation.Sum(nil)
	filterHash := sha256.Sum256([]byte(`{"q":"","kind":""}`))
	for _, change := range []struct{ old, new string }{
		{`"version":1`, `"version":2`},
		{`"user_id":"7"`, `"user_id":"8"`},
		{`"parent_id":"11"`, `"parent_id":"12"`},
		{`"scope":"directory"`, `"scope":"all"`},
		{`"sort":"modified_desc"`, `"sort":"name_asc"`},
		{hex.EncodeToString(filterHash[:]), strings.Repeat("0", 64)},
		{`2026-11-15T02:30:00.123456Z`, `2026-11-15T02:30:01.123456Z`},
		{`"entry_id":"19"`, `"entry_id":"20"`},
	} {
		changed := bytes.Replace(payload, []byte(change.old), []byte(change.new), 1)
		if bytes.Equal(changed, payload) {
			t.Fatal("篡改用例未改变载荷")
		}
		messageMAC := hmac.New(sha256.New, key)
		messageMAC.Write([]byte(base64.RawURLEncoding.EncodeToString(changed)))
		if hmac.Equal(signature, messageMAC.Sum(nil)) {
			t.Fatal("修改载荷后原签名仍匹配")
		}
	}
	rootMAC := hmac.New(sha256.New, masterKey)
	rootMAC.Write([]byte(parts[0]))
	if hmac.Equal(signature, rootMAC.Sum(nil)) {
		t.Fatal("游标签名未使用专用派生密钥")
	}
	other, err := NewDirectoryCursorSigner(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	otherCursor, err := other.Sign(7, 11, position)
	if err != nil || otherCursor == cursor {
		t.Fatal("不同主密钥生成了相同游标", err)
	}
	changedSignature := bytes.Clone(signature)
	changedSignature[0] ^= 1
	messageMAC := hmac.New(sha256.New, key)
	messageMAC.Write([]byte(parts[0]))
	if hmac.Equal(changedSignature, messageMAC.Sum(nil)) {
		t.Fatal("修改签名后仍匹配")
	}
}

func TestDirectoryCursorSignerInputs(t *testing.T) {
	for _, length := range []int{0, 1, 31} {
		if signer, err := NewDirectoryCursorSigner(make([]byte, length)); signer != nil || err == nil {
			t.Fatal("短主密钥构造成功")
		}
	}
	signer, err := NewDirectoryCursorSigner(bytes.Repeat([]byte{7}, 64))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	for _, testCase := range []struct {
		userID, parentID uint64
		position         domain.FileEntryPosition
	}{
		{0, 11, domain.FileEntryPosition{ModifiedAt: stamp, ID: 19}},
		{7, 0, domain.FileEntryPosition{ModifiedAt: stamp, ID: 19}},
		{7, 11, domain.FileEntryPosition{ModifiedAt: stamp}},
		{7, 11, domain.FileEntryPosition{ID: 19}},
		{7, 11, domain.FileEntryPosition{ModifiedAt: stamp.Add(time.Nanosecond), ID: 19}},
		{7, 11, domain.FileEntryPosition{ModifiedAt: time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC), ID: 19}},
		{7, 11, domain.FileEntryPosition{ModifiedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), ID: 19}},
	} {
		value, err := signer.Sign(testCase.userID, testCase.parentID, testCase.position)
		if value != "" || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
			t.Fatal("无效输入未返回空游标及领域错误", err)
		}
	}
	for _, invalid := range []*DirectoryCursorSigner{nil, {}, {signingKey: []byte{1}}} {
		if value, err := invalid.Sign(7, 11, domain.FileEntryPosition{ModifiedAt: stamp, ID: 19}); value != "" || err == nil {
			t.Fatal("未初始化签名器仍生成游标")
		}
	}
}

func TestDirectoryCursorSignerOwnershipAndConcurrency(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	position := domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC), ID: 19}
	expected, err := signer.Sign(7, 11, position)
	if err != nil {
		t.Fatal(err)
	}
	for index := range masterKey {
		masterKey[index] = 0
	}
	if current, err := signer.Sign(7, 11, position); err != nil || current != expected {
		t.Fatal("修改原密钥改变了已构造签名器", err)
	}
	encoded, err := json.Marshal(signer)
	if err != nil || string(encoded) != "{}" {
		t.Fatal("签名器序列化包含密钥数据", err)
	}
	type result struct {
		cursor string
		err    error
	}
	results := make(chan result, 64)
	for range 64 {
		go func() { cursor, err := signer.Sign(7, 11, position); results <- result{cursor: cursor, err: err} }()
	}
	for range 64 {
		current := <-results
		if current.err != nil || current.cursor != expected {
			t.Fatal("共享签名器并发结果异常", current.err)
		}
	}
}

func TestDirectoryCursorSignerDirectoryListResult(t *testing.T) {
	actor, reader := directoryListFixture()
	lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
		return action(ctx, reader)
	})
	page, err := lists.List(context.Background(), actor, 12, nil, 2)
	if err != nil || page == nil || !page.HasMore {
		t.Fatal("获取目录列表结果失败", err)
	}
	last := page.Entries[len(page.Entries)-1]
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := signer.Sign(actor.UserID, 12, domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
	if err != nil {
		t.Fatal(err)
	}
	fields := readDirectoryCursorForTest(t, cursor, masterKey)
	if fields["user_id"] != "7" || fields["parent_id"] != "12" || fields["entry_id"] != strconv.FormatUint(last.ID, 10) || fields["modified_at"] != last.ModifiedAt.UTC().Format("2006-01-02T15:04:05.000000Z") {
		t.Fatal("游标未绑定目录页末项")
	}
}
