package repository

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestDirectoryCursorSignerIntegration(t *testing.T) {
	// 1. 在正式表独占库中建立同时间子项，装配已有目录列表服务。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','游标签名用户','fixture_hash')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(12,1,11,'课程资料',CAST('课程资料' AS BINARY),'DIR'),(21,1,12,'子项一',CAST('子项一' AS BINARY),'DIR'),(22,1,12,'子项二',CAST('子项二' AS BINARY),'DIR'),(23,1,12,'子项三',CAST('子项三' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (21,22,23)", stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	lists := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
	})
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
	first, err := lists.List(ctx, actor, 12, nil, 2)
	if err != nil || first == nil || len(first.Entries) != 2 || !first.HasMore || first.Entries[0].ID != 23 || first.Entries[1].ID != 22 {
		t.Fatal("真实目录首页错误", err)
	}

	// 2. 使用页末项生成签名，测试内独立核对格式、HMAC 和范围字段。
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	last := first.Entries[len(first.Entries)-1]
	cursor, err := signer.Sign(actor.UserID, 12, domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		t.Fatal("真实页游标格式错误")
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
	derivation.Write([]byte("cloudnest/directory-cursor/v1"))
	messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
	messageMAC.Write([]byte(parts[0]))
	if !hmac.Equal(signature, messageMAC.Sum(nil)) {
		t.Fatal("真实页游标签名核对失败")
	}
	var payload struct {
		UserID     string `json:"user_id"`
		ParentID   string `json:"parent_id"`
		Scope      string `json:"scope"`
		Sort       string `json:"sort"`
		ModifiedAt string `json:"modified_at"`
		EntryID    string `json:"entry_id"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil || payload.UserID != "1" || payload.ParentID != "12" || payload.Scope != "directory" || payload.Sort != "modified_desc" || payload.EntryID != "22" || payload.ModifiedAt != "2026-11-15T02:30:00.123456Z" {
		t.Fatal("游标范围或页末项错误", err)
	}

	// 3. 测试内解析已核对的签名数据，将原始时间/ID 交还真实分页查询。
	modifiedAt, err := time.Parse("2006-01-02T15:04:05.000000Z", payload.ModifiedAt)
	if err != nil {
		t.Fatal(err)
	}
	entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	next, err := lists.List(ctx, actor, 12, &domain.FileEntryPosition{ModifiedAt: modifiedAt, ID: entryID}, 2)
	if err != nil || next == nil || len(next.Entries) != 1 || next.HasMore || next.Entries[0].ID != 21 {
		t.Fatal("签名数据未衔接到正确后续页", err)
	}
	if len(next.Path.Entries) != 2 || next.Path.Entries[1].ID != 12 {
		t.Fatal("后续页目录路径错误")
	}
	recreated, err := service.NewDirectoryCursorSigner(bytes.Clone(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := recreated.Sign(actor.UserID, 12, domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}); err != nil || again != cursor {
		t.Fatal("相同持久主密钥重建后游标不一致", err)
	}
}
