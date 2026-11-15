package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestDirectoryPageDTOIntegration(t *testing.T) {
	// 1. 正式表独占库中装配完整带游标目录分页服务。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','目录响应用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-directory-dto',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(12,1,11,'课程资料',CAST('课程资料' AS BINARY),'DIR'),(14,1,11,'空目录',CAST('空目录' AS BINARY),'DIR'),(21,1,12,'子项一',CAST('子项一' AS BINARY),'DIR'),(22,1,12,'子项二',CAST('子项二' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(23,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	lists := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
	})
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	pages := service.NewDirectoryPageService(verifier.Verify, lists.List, signer.Sign)
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}

	// 2. 首页映射公开字段，DTO 中的原始游标继续完成下一次真实查询。
	first, err := pages.List(ctx, actor, 12, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	data, err := dto.NewDirectoryPageData(first)
	if err != nil || data == nil || len(data.Items) != 2 || data.Items[0].ID != "23" || data.Items[0].ParentID != "12" || data.Items[0].Revision != "3" || data.Items[0].SizeBytes != 2048 || data.NextCursor == nil || len(data.Breadcrumbs) != 2 || data.Breadcrumbs[0].Name != "" || data.Breadcrumbs[1].ID != "12" {
		t.Fatal("真实首页 DTO 字段错误", err)
	}
	if data.Items[0].CreatedAt.Location() != time.UTC || !data.Items[0].ModifiedAt.Equal(stamp) {
		t.Fatal("真实时间转换错误")
	}
	response := dto.DirectoryPageResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil || len(fields) != 2 {
		t.Fatal("统一 JSON 响应外层错误")
	}
	next, err := pages.List(ctx, actor, 12, *data.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	nextData, err := dto.NewDirectoryPageData(next)
	if err != nil || nextData == nil || len(nextData.Items) != 1 || nextData.Items[0].ID != "21" || nextData.NextCursor != nil {
		t.Fatal("DTO 游标未衔接真实末页", err)
	}
	empty, err := pages.List(ctx, actor, 14, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	emptyData, err := dto.NewDirectoryPageData(empty)
	if err != nil || emptyData == nil || emptyData.Items == nil || len(emptyData.Items) != 0 || emptyData.NextCursor != nil || len(emptyData.Breadcrumbs) != 2 {
		t.Fatal("真实空目录响应错误", err)
	}

	// 3. 数据库读出的超出前端安全整数范围的字节值在转换处拒绝。
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=9007199254740992 WHERE id=23"); err != nil {
		t.Fatal(err)
	}
	unsafePage, err := pages.List(ctx, actor, 12, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if converted, err := dto.NewDirectoryPageData(unsafePage); converted != nil || err == nil {
		t.Fatal("超出 JSON 安全范围的字节值仍被交付")
	}
}
