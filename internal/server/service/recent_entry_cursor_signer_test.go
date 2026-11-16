package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func recentEntryCursorSignerFixture() ([]byte, domain.GlobalEntryQuery, domain.RecentEntryPosition) {
	masterKey, query, modifiedPosition := globalEntryCursorSignerFixture()
	query.Scope, query.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
	return masterKey, query, domain.RecentEntryPosition{LastOpenedAt: modifiedPosition.ModifiedAt, ID: modifiedPosition.ID}
}

func readRecentEntryCursorForTest(t *testing.T, cursor string, masterKey []byte) map[string]any {
	t.Helper()
	// 1. 独立拆分规范编码，核对载荷字段和长度。
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || len(cursor) > domain.MaxRecentEntryCursorLength || strings.ContainsAny(cursor, "=+/ \r\n") {
		t.Fatal("最近使用游标分段、长度或字符错误")
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != parts[0] {
		t.Fatal("最近使用游标载荷编码错误", err)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		t.Fatal("最近使用游标签名编码错误", err)
	}
	// 2. 按测试协议文字独立派生密钥，并检查 HMAC。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/recent-entry-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0]))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("最近使用游标独立 HMAC 核对失败")
	}
	var fields map[string]any
	if err := json.Unmarshal(payloadBytes, &fields); err != nil || len(fields) != 7 {
		t.Fatal("最近使用游标载荷字段错误", err)
	}
	return fields
}

func TestRecentEntryCursorSignerPayload(t *testing.T) {
	for _, mode := range []string{"canonical", "offset", "whole second", "minimum datetime", "maximum datetime", "maximum ids", "empty filters", "folder filter", "special JSON characters", "long keyword", "minimum page size", "maximum page size", "long master key"} {
		t.Run(mode, func(t *testing.T) {
			masterKey, query, position := recentEntryCursorSignerFixture()
			userID := uint64(7)
			filters := `{"q":"学习%_","kind":"document"}`
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
				filters = `{"q":"","kind":""}`
			case "folder filter":
				query.Kind = "folder"
				filters = `{"q":"学习%_","kind":"folder"}`
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
			signer, err := NewRecentEntryCursorSigner(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			originalQuery, originalPosition := query, position
			cursor, err := signer.Sign(userID, query, position)
			if err != nil || cursor == "" {
				t.Fatal(err)
			}
			fields := readRecentEntryCursorForTest(t, cursor, masterKey)
			hash := sha256.Sum256([]byte(filters))
			if fields["version"] != float64(1) || fields["user_id"] != strconv.FormatUint(userID, 10) || fields["entry_id"] != strconv.FormatUint(position.ID, 10) || fields["scope"] != "recent" || fields["sort"] != "last_opened_desc" || fields["filter_hash"] != hex.EncodeToString(hash[:]) || fields["last_opened_at"] != position.LastOpenedAt.UTC().Format("2006-01-02T15:04:05.000000Z") || query != originalQuery || position != originalPosition {
				t.Fatal("最近使用载荷、筛选摘要、访问时间或输入保护错误")
			}
			if mode == "canonical" || mode == "offset" || mode == "minimum page size" || mode == "maximum page size" {
				if strings.Split(cursor, ".")[1] != "MGBb-z7AxXj8t5sA5GgTI0RS_iVSgUAYJFbeer6OACc" || len(cursor) != 330 {
					t.Fatal("最近使用签名与独立 Python 固定向量不一致")
				}
			}
		})
	}
}

func TestRecentEntryCursorSignerBindingAndPurpose(t *testing.T) {
	masterKey, query, position := recentEntryCursorSignerFixture()
	signer, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	original, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"user", "search", "search whitespace", "kind", "entry", "microsecond", "page size"} {
		t.Run(mode, func(t *testing.T) {
			userID, changedQuery, changedPosition := uint64(7), query, position
			switch mode {
			case "user":
				userID = 8
			case "search":
				changedQuery.Search = "学习_"
			case "search whitespace":
				changedQuery.Search += " "
			case "kind":
				changedQuery.Kind = "image"
			case "entry":
				changedPosition.ID++
			case "microsecond":
				changedPosition.LastOpenedAt = changedPosition.LastOpenedAt.Add(time.Microsecond)
			case "page size":
				changedQuery.PageSize = 100
			}
			changed, err := signer.Sign(userID, changedQuery, changedPosition)
			if err != nil || (changed == original) != (mode == "page size") {
				t.Fatal("最近使用的用户、筛选、访问位置或页大小绑定错误", err)
			}
		})
	}
	for _, pair := range [][2]string{{"alpha", "ALPHA"}, {"Café", "Cafe\u0301"}} {
		firstQuery, secondQuery := query, query
		firstQuery.Search, secondQuery.Search = pair[0], pair[1]
		first, firstError := signer.Sign(7, firstQuery, position)
		second, secondError := signer.Sign(7, secondQuery, position)
		if firstError != nil || secondError != nil || first == second {
			t.Fatal("搜索原文的大小写或 Unicode 表达未独立绑定")
		}
	}
	// 1. 同一个测试主密钥下，各用途派生出的 HMAC 分开验证。
	parts := strings.Split(original, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{"cloudnest/global-entry-cursor/v1", "cloudnest/directory-cursor/v1"} {
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte(purpose))
		messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
		messageMAC.Write([]byte(parts[0]))
		if hmac.Equal(signature, messageMAC.Sum(nil)) {
			t.Fatal("最近使用游标与其他用途密钥混用")
		}
	}
	// 2. 既有校验器拒绝最近使用游标。
	globalVerifier, err := NewGlobalEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	globalQuery := query
	globalQuery.Scope, globalQuery.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortModifiedDesc
	if value, err := globalVerifier.Verify(7, globalQuery, original); value != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) {
		t.Fatal("全局校验器接受最近使用游标")
	}
	directoryVerifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := directoryVerifier.Verify(7, 11, original); value != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
		t.Fatal("目录校验器接受最近使用游标")
	}
	// 3. 在测试内检查载荷与签名字节修改会使 HMAC 不匹配。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/recent-entry-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0] + "A"))
	if hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("载荷变化后最近使用签名仍匹配")
	}
	messageMAC.Reset()
	messageMAC.Write([]byte(parts[0]))
	signature[0] ^= 1
	if hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("签名字节变化后仍匹配")
	}
}

func TestRecentEntryCursorSignerInputs(t *testing.T) {
	for _, length := range []int{0, 1, 31} {
		if signer, err := NewRecentEntryCursorSigner(make([]byte, length)); signer != nil || err == nil {
			t.Fatal("最近使用短主密钥构造成功")
		}
	}
	masterKey, query, position := recentEntryCursorSignerFixture()
	signer, err := NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"zero user", "zero entry", "zero time", "early date", "late date", "nanosecond", "empty query", "all scope", "starred scope", "modified sort", "name sort", "size sort", "invalid search", "invalid kind", "zero page", "large page"} {
		t.Run(mode, func(t *testing.T) {
			userID, changedQuery, changedPosition := uint64(7), query, position
			switch mode {
			case "zero user":
				userID = 0
			case "zero entry":
				changedPosition.ID = 0
			case "zero time":
				changedPosition.LastOpenedAt = time.Time{}
			case "early date":
				changedPosition.LastOpenedAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
			case "late date":
				changedPosition.LastOpenedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "nanosecond":
				changedPosition.LastOpenedAt = changedPosition.LastOpenedAt.Add(time.Nanosecond)
			case "empty query":
				changedQuery = domain.GlobalEntryQuery{}
			case "all scope", "starred scope":
				changedQuery.Scope, changedQuery.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortModifiedDesc
				if mode == "starred scope" {
					changedQuery.Scope = domain.GlobalEntryScopeStarred
				}
			case "modified sort":
				changedQuery.Sort = domain.GlobalEntrySortModifiedDesc
			case "name sort":
				changedQuery.Sort = domain.GlobalEntrySortNameAsc
			case "size sort":
				changedQuery.Sort = domain.GlobalEntrySortSizeDesc
			case "invalid search":
				changedQuery.Search = string([]byte{0xff})
			case "invalid kind":
				changedQuery.Kind = "private-kind"
			case "zero page":
				changedQuery.PageSize = 0
			case "large page":
				changedQuery.PageSize = 101
			}
			if cursor, err := signer.Sign(userID, changedQuery, changedPosition); cursor != "" || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) || strings.Contains(err.Error(), "private-") {
				t.Fatal("非法最近使用游标输入的结果错误", err)
			}
		})
	}
	for _, broken := range []*RecentEntryCursorSigner{nil, {}, {signingKey: []byte{1}}} {
		if cursor, err := broken.Sign(7, query, position); cursor != "" || err == nil {
			t.Fatal("未初始化的最近使用签名器返回游标")
		}
	}
}

func TestRecentEntryCursorSignerOwnershipAndConcurrency(t *testing.T) {
	masterKey, query, position := recentEntryCursorSignerFixture()
	originalKey := bytes.Clone(masterKey)
	signer, err := NewRecentEntryCursorSigner(masterKey)
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
	recreated, err := NewRecentEntryCursorSigner(originalKey)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := recreated.Sign(7, query, position); err != nil || again != expected {
		t.Fatal("相同最近使用主密钥重建后签名不同", err)
	}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				if cursor, err := signer.Sign(7, query, position); err != nil || cursor != expected {
					t.Error("主密钥输入修改或并发影响最近使用签名", err)
				}
			}
		})
	}
	workers.Wait()
}
