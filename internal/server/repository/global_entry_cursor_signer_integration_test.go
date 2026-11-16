package repository

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestGlobalEntryCursorSignerIntegration(t *testing.T) {
	// 1. 在正式表中准备预算空页及可见续页，装配全局列表和签名器。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','全局签名用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-global-cursor-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(12,1,11,'活动目录',CAST('活动目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(14,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'学习%_笔记.md',CAST('学习%_笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048),(15,1,12,'学习%_计划.md',CAST('学习%_计划.md' AS BINARY),'FILE','DOCUMENT',41,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立全局签名夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=?", stamp); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("学习%%_被隐藏-%d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(?,1,14,?,?,'FILE','DOCUMENT',41,2048,?)", 1000+index, name, []byte(name), stamp.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	listing := service.NewGlobalEntryListService(func(current context.Context, action func(context.Context, service.GlobalEntryListReader) error) error {
		return store.WithFileEntrySnapshot(current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	query := globalEntryCandidateQuery(2)
	query.Search, query.Kind = "学习%_", "document"
	first, err := listing.List(ctx, actor, query, nil)
	if err != nil || first == nil || first.Entries == nil || len(first.Entries) != 0 || !first.HasMore || first.NextPosition == nil || first.NextPosition.ID != 1001 {
		t.Fatal("真实预算空页位置错误", err)
	}

	// 2. 对真实空页位置签名，独立核对载荷、摘要与 HMAC。
	readPosition := func(cursor string) domain.FileEntryPosition {
		t.Helper()
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || len(cursor) > domain.MaxGlobalEntryCursorLength {
			t.Fatal("真实全局游标格式错误")
		}
		payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte("cloudnest/global-entry-cursor/v1"))
		mac := hmac.New(sha256.New, derivation.Sum(nil))
		mac.Write([]byte(parts[0]))
		if !hmac.Equal(signature, mac.Sum(nil)) {
			t.Fatal("真实全局游标 HMAC 核对失败")
		}
		var payload struct {
			Version    uint8  `json:"version"`
			UserID     string `json:"user_id"`
			Scope      string `json:"scope"`
			Sort       string `json:"sort"`
			FilterHash string `json:"filter_hash"`
			ModifiedAt string `json:"modified_at"`
			EntryID    string `json:"entry_id"`
		}
		filters := sha256.Sum256([]byte(`{"q":"学习%_","kind":"document"}`))
		if json.Unmarshal(payloadBytes, &payload) != nil || payload.Version != 1 || payload.UserID != "1" || payload.Scope != "all" || payload.Sort != "modified_desc" || payload.FilterHash != hex.EncodeToString(filters[:]) {
			t.Fatal("真实全局载荷或筛选摘要错误")
		}
		entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		modifiedAt, err := time.Parse("2006-01-02T15:04:05.000000Z", payload.ModifiedAt)
		if err != nil {
			t.Fatal(err)
		}
		position := domain.FileEntryPosition{ModifiedAt: modifiedAt, ID: entryID}
		if !position.IsValid() {
			t.Fatal("签名位置数据错误")
		}
		return position
	}
	cursor, err := signer.Sign(actor.UserID, query, *first.NextPosition)
	if err != nil {
		t.Fatal(err)
	}
	position := readPosition(cursor)
	if position != *first.NextPosition {
		t.Fatal("签名未保留空页最后消费位置")
	}

	// 3. 从测试内已核对的位置继续分页，验证页大小调整和稳定签名。
	query.PageSize = 1
	if resized, err := signer.Sign(actor.UserID, query, position); err != nil || resized != cursor {
		t.Fatal("合法页大小调整改变了查询绑定", err)
	}
	second, err := listing.List(ctx, actor, query, &position)
	if err != nil || second == nil || len(second.Entries) != 1 || second.Entries[0].ID != 15 || !second.HasMore || second.NextPosition == nil || second.NextPosition.ID != 15 {
		t.Fatal("签名空页位置未衔接可见续页", err)
	}
	secondCursor, err := signer.Sign(actor.UserID, query, *second.NextPosition)
	if err != nil {
		t.Fatal(err)
	}
	secondPosition := readPosition(secondCursor)
	third, err := listing.List(ctx, actor, query, &secondPosition)
	if err != nil || third == nil || len(third.Entries) != 1 || third.Entries[0].ID != 13 || third.HasMore || third.NextPosition != nil {
		t.Fatal("真实签名续页漏项或结束状态错误", err)
	}
	recreated, err := service.NewGlobalEntryCursorSigner(bytes.Clone(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := recreated.Sign(actor.UserID, query, position); err != nil || again != cursor {
		t.Fatal("相同主密钥重建后签名不稳定", err)
	}
}
