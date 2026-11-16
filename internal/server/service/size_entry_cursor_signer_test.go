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

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func sizeEntryCursorSignerFixture() ([]byte, domain.GlobalEntryQuery, domain.SizeEntryPosition) {
	masterKey := make([]byte, 32)
	for index := range masterKey {
		masterKey[index] = byte(index)
	}
	query := domain.GlobalEntryQuery{Scope: "all", Sort: "size_desc", Search: "学习%_", Kind: "document", PageSize: 50}
	return masterKey, query, domain.SizeEntryPosition{FileSize: 2048, ID: 19}
}

func readSizeEntryCursorForTest(t *testing.T, cursor string, masterKey []byte) map[string]any {
	t.Helper()
	// 1. 独立拆分规范编码，核对载荷字段和长度。
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || len(cursor) > domain.MaxSizeEntryCursorLength || strings.ContainsAny(cursor, "=+/ \r\n") {
		t.Fatal("大小降序游标分段、长度或字符错误")
	}
	payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != parts[0] {
		t.Fatal("大小降序游标载荷编码错误", err)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		t.Fatal("大小降序游标签名编码错误", err)
	}
	// 2. 按测试协议文字独立派生密钥，并检查 HMAC。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/size-entry-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0]))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("大小降序游标独立 HMAC 核对失败")
	}
	var fields map[string]any
	if err := json.Unmarshal(payloadBytes, &fields); err != nil || len(fields) != 7 {
		t.Fatal("大小降序游标载荷字段错误", err)
	}
	return fields
}

func TestSizeEntryCursorSignerPayload(t *testing.T) {
	for _, mode := range []string{"canonical", "starred", "zero size", "one byte", "JSON safe limit", "above JSON safe limit", "signed limit", "above signed limit", "maximum size", "maximum IDs", "maximum payload", "empty filters", "folder filter", "special JSON characters", "long keyword", "minimum page size", "maximum page size", "long master key"} {
		t.Run(mode, func(t *testing.T) {
			masterKey, query, position := sizeEntryCursorSignerFixture()
			userID := uint64(7)
			filters := `{"q":"学习%_","kind":"document"}`
			switch mode {
			case "starred":
				query.Scope = "starred"
			case "zero size":
				position.FileSize = 0
			case "one byte":
				position.FileSize = 1
			case "JSON safe limit":
				position.FileSize = 1<<53 - 1
			case "above JSON safe limit":
				position.FileSize = 1 << 53
			case "signed limit":
				position.FileSize = 1<<63 - 1
			case "above signed limit":
				position.FileSize = 1 << 63
			case "maximum size":
				position.FileSize = ^uint64(0)
			case "maximum IDs":
				userID, position.ID = ^uint64(0), ^uint64(0)
			case "maximum payload":
				userID, position.ID, position.FileSize, query.Scope = ^uint64(0), ^uint64(0), ^uint64(0), "starred"
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
			signer, err := NewSizeEntryCursorSigner(masterKey)
			if err != nil {
				t.Fatal(err)
			}
			originalQuery := query
			originalPosition := copyOptionalValue(&position)
			cursor, err := signer.Sign(userID, query, position)
			if err != nil || cursor == "" {
				t.Fatal(err)
			}
			fields := readSizeEntryCursorForTest(t, cursor, masterKey)
			hash := sha256.Sum256([]byte(filters))
			if fields["version"] != float64(1) || fields["user_id"] != strconv.FormatUint(userID, 10) || fields["entry_id"] != strconv.FormatUint(position.ID, 10) || fields["scope"] != query.Scope || fields["sort"] != "size_desc" || fields["filter_hash"] != hex.EncodeToString(hash[:]) || fields["file_size"] != strconv.FormatUint(position.FileSize, 10) || query != originalQuery || position != *originalPosition {
				t.Fatal("大小载荷、完整数值、筛选摘要或输入保护错误")
			}
			if mode == "canonical" || mode == "minimum page size" || mode == "maximum page size" {
				if cursor != "eyJ2ZXJzaW9uIjoxLCJ1c2VyX2lkIjoiNyIsInNjb3BlIjoiYWxsIiwic29ydCI6InNpemVfZGVzYyIsImZpbHRlcl9oYXNoIjoiNGExOTg0N2FhMjE2NTk5ZDliMTUzZjNmNmFjMTdhNjVlOGQ5ZTNiYmZlOGUxYzU0OWQ1NjcwNTUwMGIzMmIyMiIsImZpbGVfc2l6ZSI6IjIwNDgiLCJlbnRyeV9pZCI6IjE5In0.VjM56TElh8stM6vLzic-UPro2WGXGyX4ciHuKEGiqyk" || len(cursor) != 279 {
					t.Fatal("大小签名与独立 Python 固定向量不符")
				}
			}
			if mode == "maximum payload" && (len(cursor) != 355 || len(cursor) > domain.MaxSizeEntryCursorLength) {
				t.Fatal("最大大小、用户和目录项 ID 的完整游标上界错误")
			}
		})
	}
}

func TestSizeEntryCursorSignerBindingAndPurpose(t *testing.T) {
	masterKey, query, position := sizeEntryCursorSignerFixture()
	signer, err := NewSizeEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	original, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"user", "search", "search whitespace", "kind", "entry", "file size", "scope", "page size"} {
		t.Run(mode, func(t *testing.T) {
			userID, changedQuery, changedPosition := uint64(7), query, *copyOptionalValue(&position)
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
			case "file size":
				changedPosition.FileSize++
			case "scope":
				changedQuery.Scope = "starred"
			case "page size":
				changedQuery.PageSize = 100
			}
			changed, err := signer.Sign(userID, changedQuery, changedPosition)
			if err != nil || (changed == original) != (mode == "page size") {
				t.Fatal("大小降序的用户、筛选、大小位置或页大小绑定错误", err)
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
	for _, purpose := range []string{"cloudnest/global-entry-cursor/v1", "cloudnest/directory-cursor/v1", "cloudnest/recent-entry-cursor/v1", "cloudnest/name-entry-cursor/v1"} {
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte(purpose))
		messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
		messageMAC.Write([]byte(parts[0]))
		if hmac.Equal(signature, messageMAC.Sum(nil)) {
			t.Fatal("大小降序游标与其他用途密钥混用")
		}
	}
	// 2. 既有校验器拒绝大小降序游标。
	globalVerifier, err := NewGlobalEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	globalQuery := query
	globalQuery.Scope, globalQuery.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortModifiedDesc
	if value, err := globalVerifier.Verify(7, globalQuery, original); value != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) {
		t.Fatal("全局校验器接受大小降序游标")
	}
	directoryVerifier, err := NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := directoryVerifier.Verify(7, 11, original); value != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) {
		t.Fatal("目录校验器接受大小降序游标")
	}
	recentVerifier, err := NewRecentEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	recentQuery := query
	recentQuery.Scope, recentQuery.Sort = "recent", "last_opened_desc"
	if value, err := recentVerifier.Verify(7, recentQuery, original); value != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
		t.Fatal("最近校验器接受大小游标")
	}
	nameVerifier, err := NewNameEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	nameQuery := query
	nameQuery.Sort = "name_asc"
	if value, err := nameVerifier.Verify(7, nameQuery, original); value != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
		t.Fatal("名称校验器接受大小游标")
	}
	// 3. 在测试内检查载荷与签名字节修改会使 HMAC 不匹配。
	derivation := hmac.New(sha256.New, masterKey)
	derivation.Write([]byte("cloudnest/size-entry-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0] + "A"))
	if hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("载荷变化后大小降序签名仍匹配")
	}
	messageMAC.Reset()
	messageMAC.Write([]byte(parts[0]))
	signature[0] ^= 1
	if hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("签名字节变化后仍匹配")
	}
}

func TestSizeEntryCursorSignerInputs(t *testing.T) {
	for _, length := range []int{0, 1, 31} {
		if signer, err := NewSizeEntryCursorSigner(make([]byte, length)); signer != nil || err == nil {
			t.Fatal("大小短主密钥构造成功")
		}
	}
	masterKey, query, position := sizeEntryCursorSignerFixture()
	signer, err := NewSizeEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"zero user", "zero entry", "empty query", "directory scope", "recent scope", "uppercase scope", "modified sort", "name sort", "invalid search", "invalid kind", "uppercase kind", "zero page", "large page"} {
		t.Run(mode, func(t *testing.T) {
			userID, changedQuery, changedPosition := uint64(7), query, *copyOptionalValue(&position)
			switch mode {
			case "zero user":
				userID = 0
			case "zero entry":
				changedPosition.ID = 0
			case "empty query":
				changedQuery = domain.GlobalEntryQuery{}
			case "directory scope":
				changedQuery.Scope = "directory"
			case "recent scope":
				changedQuery.Scope, changedQuery.Sort = "recent", "last_opened_desc"
			case "uppercase scope":
				changedQuery.Scope = "ALL"
			case "modified sort":
				changedQuery.Sort = "modified_desc"
			case "name sort":
				changedQuery.Sort = "name_asc"
			case "invalid search":
				changedQuery.Search = string([]byte{0xff})
			case "invalid kind":
				changedQuery.Kind = "private-kind"
			case "uppercase kind":
				changedQuery.Kind = "DOCUMENT"
			case "zero page":
				changedQuery.PageSize = 0
			case "large page":
				changedQuery.PageSize = 101
			}
			if cursor, err := signer.Sign(userID, changedQuery, changedPosition); cursor != "" || !errors.Is(err, domain.ErrInvalidSizeEntryCursor) || strings.Contains(err.Error(), "private-") {
				t.Fatal("非法大小签名输入的结果错误", err)
			}
		})
	}
	for _, broken := range []*SizeEntryCursorSigner{nil, {}, {signingKey: []byte{1}}, {signingKey: make([]byte, 33)}} {
		if cursor, err := broken.Sign(7, query, position); cursor != "" || err == nil {
			t.Fatal("未初始化大小签名器返回游标")
		}
	}
}

func TestSizeEntryCursorSignerOwnershipAndConcurrency(t *testing.T) {
	masterKey, query, position := sizeEntryCursorSignerFixture()
	originalKey := bytes.Clone(masterKey)
	signer, err := NewSizeEntryCursorSigner(masterKey)
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
	recreated, err := NewSizeEntryCursorSigner(originalKey)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := recreated.Sign(7, query, position); err != nil || again != expected {
		t.Fatal("相同大小降序主密钥重建后签名不同", err)
	}
	if encoded, err := json.Marshal(signer); err != nil || string(encoded) != "{}" {
		t.Fatal("签名器公开了私有密钥")
	}
	savedPosition := copyOptionalValue(&position)
	position.FileSize++
	if again, err := signer.Sign(7, query, position); err != nil || again == expected {
		t.Fatal("来源位置修改未产生新的大小游标")
	}
	position = *savedPosition
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				if cursor, err := signer.Sign(7, query, position); err != nil || cursor != expected {
					t.Error("主密钥输入修改或并发影响大小降序签名", err)
				}
			}
		})
	}
	workers.Wait()
}
