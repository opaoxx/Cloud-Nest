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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func readGlobalEntryCursorForTest(t *testing.T, cursor string, masterKey []byte) map[string]any {
	t.Helper()
	// 1. 独立解析规范 Base64URL，检查七字段载荷及长度。
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || len(cursor) > domain.MaxGlobalEntryCursorLength || strings.ContainsAny(cursor, "=+/ \r\n") {
		t.Fatal("全局游标分段、长度或字符错误")
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != parts[0] {
		t.Fatal("全局载荷编码错误", err)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		t.Fatal("全局签名编码错误", err)
	}
	// 2. 从测试主密钥和用途文字重算 HMAC。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/global-entry-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0]))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("全局游标独立 HMAC 核对失败")
	}
	var fields map[string]any
	if err := json.Unmarshal(payloadBytes, &fields); err != nil || len(fields) != 7 {
		t.Fatal("全局载荷字段错误", err)
	}
	return fields
}

func globalEntryCursorSignerFixture() ([]byte, domain.GlobalEntryQuery, domain.FileEntryPosition) {
	masterKey := make([]byte, 32)
	for index := range masterKey {
		masterKey[index] = byte(index)
	}
	query := domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortModifiedDesc, Search: "学习%_", Kind: "document", PageSize: 50}
	position := domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC), ID: 19}
	return masterKey, query, position
}

func TestGlobalEntryCursorSignerPayload(t *testing.T) {
	for _, mode := range []string{"canonical", "offset", "whole second", "minimum datetime", "maximum datetime", "maximum ids", "empty filters", "special JSON characters", "long keyword", "minimum page size", "maximum page size", "long master key"} {
		t.Run(mode, func(t *testing.T) {
			masterKey, query, position := globalEntryCursorSignerFixture()
			userID := uint64(7)
			filters := `{"q":"学习%_","kind":"document"}`
			switch mode {
			case "offset":
				position.ModifiedAt = position.ModifiedAt.In(time.FixedZone("fixture", 8*3600))
			case "whole second":
				position.ModifiedAt = position.ModifiedAt.Truncate(time.Second)
			case "minimum datetime":
				position.ModifiedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "maximum datetime":
				position.ModifiedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "maximum ids":
				userID, position.ID = ^uint64(0), ^uint64(0)
			case "empty filters":
				query.Search, query.Kind = "", ""
				filters = `{"q":"","kind":""}`
			case "special JSON characters":
				query.Search = "<>&\"\\\n\x00\u2028"
				filters = `{"q":"\u003c\u003e\u0026\"\\\n\u0000\u2028","kind":"document"}`
			case "long keyword":
				query.Search = strings.Repeat("界", 2048)
				filters = `{"q":"` + query.Search + `","kind":"document"}`
			case "minimum page size":
				query.PageSize = 1
			case "maximum page size":
				query.PageSize = 100
			case "long master key":
				masterKey = bytes.Repeat([]byte{9}, 64)
			}
			signer, err := NewGlobalEntryCursorSigner(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			originalQuery, originalPosition := query, position
			cursor, err := signer.Sign(userID, query, position)
			if err != nil || cursor == "" {
				t.Fatal(err)
			}
			fields := readGlobalEntryCursorForTest(t, cursor, masterKey)
			hash := sha256.Sum256([]byte(filters))
			if fields["version"] != float64(1) || fields["user_id"] != strconv.FormatUint(userID, 10) || fields["entry_id"] != strconv.FormatUint(position.ID, 10) || fields["scope"] != "all" || fields["sort"] != "modified_desc" || fields["filter_hash"] != hex.EncodeToString(hash[:]) || fields["modified_at"] != position.ModifiedAt.UTC().Format("2006-01-02T15:04:05.000000Z") || !reflect.DeepEqual(position, originalPosition) || query != originalQuery {
				t.Fatal("全局载荷、筛选摘要、时间或输入保护错误")
			}
			if mode == "canonical" || mode == "offset" || mode == "minimum page size" || mode == "maximum page size" {
				if strings.Split(cursor, ".")[1] != "qPAFnAylyXFwHZrDjqz1IidIjR1z49Od8-X2-NhMGyI" {
					t.Fatal("签名与独立 Python 固定向量不一致")
				}
			}
		})
	}
}

func TestGlobalEntryCursorSignerBindingAndPurpose(t *testing.T) {
	masterKey, query, position := globalEntryCursorSignerFixture()
	signer, err := NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	original, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"user", "search", "search whitespace", "search suffix", "kind", "entry", "microsecond", "page size"} {
		t.Run(mode, func(t *testing.T) {
			userID, changedQuery, changedPosition := uint64(7), query, position
			switch mode {
			case "user":
				userID = 8
			case "search":
				changedQuery.Search = "学习_"
			case "search whitespace":
				changedQuery.Search += " "
			case "search suffix":
				changedQuery.Search += "A"
			case "kind":
				changedQuery.Kind = "image"
			case "entry":
				changedPosition.ID++
			case "microsecond":
				changedPosition.ModifiedAt = changedPosition.ModifiedAt.Add(time.Microsecond)
			case "page size":
				changedQuery.PageSize = 100
			}
			changed, err := signer.Sign(userID, changedQuery, changedPosition)
			if err != nil || (changed == original) != (mode == "page size") {
				t.Fatal("用户、筛选、位置或每页数量绑定错误", err)
			}
		})
	}
	for _, pair := range [][2]string{{"alpha", "ALPHA"}, {"Café", "Cafe\u0301"}} {
		firstQuery, secondQuery := query, query
		firstQuery.Search, secondQuery.Search = pair[0], pair[1]
		first, firstError := signer.Sign(7, firstQuery, position)
		second, secondError := signer.Sign(7, secondQuery, position)
		if firstError != nil || secondError != nil || first == second {
			t.Fatal("搜索原文大小写或 Unicode 形式未独立绑定")
		}
	}
	// 1. 检查独立用途密钥和既有目录校验器边界。
	verifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := verifier.Verify(7, 11, original); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
		t.Fatal("目录校验器接受了全局游标")
	}
	// 2. 在测试内独立检验载荷或签名字节被修改后的 HMAC。
	parts := strings.Split(original, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/global-entry-cursor/v1"))
	mac := hmac.New(sha256.New, derivation.Sum(nil))
	mac.Write([]byte(parts[0] + "A"))
	if hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("修改载荷后签名仍匹配")
	}
	mac.Reset()
	mac.Write([]byte(parts[0]))
	signature[0] ^= 1
	if hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("修改签名字节后仍匹配")
	}
}

func TestGlobalEntryCursorSignerInputs(t *testing.T) {
	for _, length := range []int{0, 1, 31} {
		if signer, err := NewGlobalEntryCursorSigner(make([]byte, length)); signer != nil || err == nil {
			t.Fatal("短主密钥构造成功")
		}
	}
	masterKey, query, position := globalEntryCursorSignerFixture()
	signer, err := NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"zero user", "zero entry", "zero time", "early date", "late date", "nanosecond", "empty query", "starred scope", "recent scope", "name sort", "size sort", "invalid search", "invalid kind", "zero page", "large page"} {
		t.Run(mode, func(t *testing.T) {
			userID, inputQuery, inputPosition := uint64(7), query, position
			switch mode {
			case "zero user":
				userID = 0
			case "zero entry":
				inputPosition.ID = 0
			case "zero time":
				inputPosition.ModifiedAt = time.Time{}
			case "early date":
				inputPosition.ModifiedAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
			case "late date":
				inputPosition.ModifiedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "nanosecond":
				inputPosition.ModifiedAt = inputPosition.ModifiedAt.Add(time.Nanosecond)
			case "empty query":
				inputQuery = domain.GlobalEntryQuery{}
			case "starred scope":
				inputQuery.Scope = domain.GlobalEntryScopeStarred
			case "recent scope":
				inputQuery.Scope, inputQuery.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
			case "name sort":
				inputQuery.Sort = domain.GlobalEntrySortNameAsc
			case "size sort":
				inputQuery.Sort = domain.GlobalEntrySortSizeDesc
			case "invalid search":
				inputQuery.Search = string([]byte{0xff})
			case "invalid kind":
				inputQuery.Kind = "private-kind"
			case "zero page":
				inputQuery.PageSize = 0
			case "large page":
				inputQuery.PageSize = 101
			}
			cursor, err := signer.Sign(userID, inputQuery, inputPosition)
			if cursor != "" || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) || strings.Contains(err.Error(), "private-") {
				t.Fatal("非法游标输入结果错误", err)
			}
		})
	}
	for _, broken := range []*GlobalEntryCursorSigner{nil, {}, {signingKey: []byte{1}}} {
		if cursor, err := broken.Sign(7, query, position); cursor != "" || err == nil {
			t.Fatal("未初始化签名器返回了游标")
		}
	}
}

func TestGlobalEntryCursorSignerOwnershipAndConcurrency(t *testing.T) {
	masterKey, query, position := globalEntryCursorSignerFixture()
	originalKey := bytes.Clone(masterKey)
	signer, err := NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	for index := range masterKey {
		masterKey[index] = 0
	}
	recreated, err := NewGlobalEntryCursorSigner(originalKey)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := recreated.Sign(7, query, position); err != nil || again != expected {
		t.Fatal("相同主密钥重建后结果不一致", err)
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				if cursor, err := signer.Sign(7, query, position); err != nil || cursor != expected {
					t.Error("主密钥输入修改或并发调用影响签名", err)
				}
			}
		})
	}
	workers.Wait()
}
