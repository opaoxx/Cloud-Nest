package repository

import (
	"context"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestResolveUploadPublicationNameIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 目标目录同时有活动和回收同名项，其他目录及用户有相同候选。
	for _, entry := range []struct {
		userID, parentID uint64
		name, status     string
	}{
		{7, 13, "Café.txt", "ACTIVE"},
		{7, 13, "CAFÉ (2).TXT", "TRASHED"},
		{8, 14, "CAFÉ (2).TXT", "ACTIVE"},
	} {
		key, err := service.BuildEntryNameKey(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.ExecContext(ctx, `INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type,status,deleted_at)
			VALUES(?,?,?,?,'DIR',?,CASE WHEN ?='TRASHED' THEN UTC_TIMESTAMP(6) END)`,
			entry.userID, entry.parentID, entry.name, key, entry.status, entry.status)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(15,7,13,'子目录',CAST('子目录' AS BINARY),'DIR')"); err != nil {
		t.Fatal(err)
	}

	// 2. 在用户锁内选择发布名称，验证文件与目录共用活动名称空间。
	const original = "Cafe\u0301.txt"
	err = store.withActiveUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
		check := func(parentID uint64, want string) {
			t.Helper()
			actual, err := service.ResolveUploadPublicationName(txContext, 7, parentID, original, txStore.FindUserActiveFileEntryByName)
			if err != nil || actual != want {
				t.Fatalf("目录 %d 返回 %q，期望 %q，错误 %v", parentID, actual, want, err)
			}
		}
		check(13, "Cafe\u0301 (1).txt")
		check(15, original)

		// 3. 同一事务写入活动文件后重新选名，须跳过刚占用的 (1)。
		const occupiedName = "CAFÉ (1).TXT"
		key, err := service.BuildEntryNameKey(occupiedName)
		if err != nil {
			return err
		}
		result := txStore.database.WithContext(txContext).Exec(
			"INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(7,13,?,?,'FILE','DOCUMENT',29,16777216)",
			occupiedName, key)
		if result.Error != nil {
			return result.Error
		}
		check(13, "Cafe\u0301 (2).txt")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
