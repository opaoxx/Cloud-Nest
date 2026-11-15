package repository

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestDirectoryPageServiceIntegration(t *testing.T) {
	// 1. 在正式表独占库中组合真实列表、签名、校验和带游标分页服务。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','完整目录分页用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-directory-page',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'课程资料',CAST('课程资料' AS BINARY),'DIR'),(14,1,11,'空目录',CAST('空目录' AS BINARY),'DIR'),(21,1,12,'子项一',CAST('子项一' AS BINARY),'DIR'),(22,1,12,'子项二',CAST('子项二' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(23,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(42,1,11,'根下文件.md',CAST('根下文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=?", stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (22,23)", stamp.Add(time.Second)); err != nil {
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
	verifyCalls, listCalls, signCalls := 0, 0, 0
	pages := service.NewDirectoryPageService(
		func(userID, parentID uint64, text string) (*domain.FileEntryPosition, error) {
			verifyCalls++
			return verifier.Verify(userID, parentID, text)
		},
		func(ctx context.Context, actor domain.Actor, parentID uint64, before *domain.FileEntryPosition, size int) (*domain.DirectoryList, error) {
			listCalls++
			return lists.List(ctx, actor, parentID, before, size)
		},
		func(userID, parentID uint64, position domain.FileEntryPosition) (string, error) {
			signCalls++
			return signer.Sign(userID, parentID, position)
		},
	)
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}

	// 2. 首页生成真实 next_cursor，后续页、空页和满额末页返回 nil。
	first, err := pages.List(ctx, actor, 12, "", 2)
	if err != nil || first == nil || len(first.Entries) != 2 || first.Entries[0].ID != 23 || first.Entries[1].ID != 22 || first.NextCursor == nil || verifyCalls != 0 || listCalls != 1 || signCalls != 1 {
		t.Fatal("完整目录首页或调用次数错误", err)
	}
	if position, err := verifier.Verify(1, 12, *first.NextCursor); err != nil || position == nil || position.ID != 22 || !position.ModifiedAt.Equal(stamp.Add(time.Second)) {
		t.Fatal("首页下一页游标未绑定末项", err)
	}
	second, err := pages.List(ctx, actor, 12, *first.NextCursor, 2)
	if err != nil || second == nil || len(second.Entries) != 1 || second.Entries[0].ID != 21 || second.NextCursor != nil || verifyCalls != 1 || listCalls != 2 || signCalls != 1 || len(second.Path.Entries) != 2 || second.Path.Entries[1].ID != 12 {
		t.Fatal("客户端游标未衔接真实末页", err)
	}
	lastCursor, err := signer.Sign(1, 12, domain.FileEntryPosition{ModifiedAt: second.Entries[0].ModifiedAt, ID: 21})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := pages.List(ctx, actor, 12, lastCursor, 2)
	if err != nil || empty == nil || empty.Entries == nil || len(empty.Entries) != 0 || empty.NextCursor != nil {
		t.Fatal("游标后的空页错误", err)
	}
	full, err := pages.List(ctx, actor, 12, "", 3)
	if err != nil || full == nil || len(full.Entries) != 3 || full.NextCursor != nil {
		t.Fatal("满额末页错误", err)
	}
	emptyDirectory, err := pages.List(ctx, actor, 14, "", 2)
	if err != nil || emptyDirectory == nil || len(emptyDirectory.Entries) != 0 || emptyDirectory.NextCursor != nil {
		t.Fatal("空目录分页错误", err)
	}
	root, err := pages.List(ctx, actor, 11, "", 100)
	if err != nil || root == nil || len(root.Path.Entries) != 1 || len(root.Entries) != 3 || root.NextCursor != nil {
		t.Fatal("根目录分页错误", err)
	}

	// 3. 客户端游标错误在目录查询前停止，数据库状态错误保留原分类。
	for _, testCase := range []struct {
		userID, parentID uint64
		cursor           string
	}{
		{3, 12, *first.NextCursor}, {1, 14, *first.NextCursor}, {1, 12, "invalid-cursor"}, {1, 12, *first.NextCursor + ".extra"}, {1, 12, strings.Repeat("x", domain.MaxDirectoryCursorLength+1)},
	} {
		identity := actor
		identity.UserID = testCase.userID
		beforeLists, beforeSigns := listCalls, signCalls
		if result, err := pages.List(ctx, identity, testCase.parentID, testCase.cursor, 2); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) || listCalls != beforeLists || signCalls != beforeSigns {
			t.Fatal("错误游标仍进入列表或签名", err)
		}
	}
	for _, testCase := range []struct {
		parentID uint64
		want     error
	}{{999, domain.ErrFileEntryNotFound}, {33, domain.ErrFileEntryNotFound}, {42, domain.ErrFileEntryNotDirectory}} {
		if result, err := pages.List(ctx, actor, testCase.parentID, "", 2); result != nil || !errors.Is(err, testCase.want) {
			t.Fatal("目录状态错误未保留", err)
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if result, err := pages.List(ctx, actor, 12, *first.NextCursor, 2); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
		t.Fatal("禁用账号仍取得分页结果", err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
		t.Fatal(err)
	}
	if result, err := pages.List(ctx, actor, 12, *first.NextCursor, 2); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
		t.Fatal("回收目录仍取得分页结果", err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=12"); err != nil {
		t.Fatal(err)
	}

	// 4. 已读取目录后签名失败或取消时丢弃结果，随后仍可正常读取。
	failure := errors.New("fixture signing failure")
	failing := service.NewDirectoryPageService(verifier.Verify, lists.List, func(uint64, uint64, domain.FileEntryPosition) (string, error) { return "", failure })
	if result, err := failing.List(ctx, actor, 12, "", 2); result != nil || !errors.Is(err, failure) {
		t.Fatal("签名失败仍返回部分目录数据", err)
	}
	requestContext, stop := context.WithCancel(ctx)
	defer stop()
	cancelling := service.NewDirectoryPageService(verifier.Verify, lists.List, func(userID, parentID uint64, position domain.FileEntryPosition) (string, error) {
		text, err := signer.Sign(userID, parentID, position)
		stop()
		return text, err
	})
	if result, err := cancelling.List(requestContext, actor, 12, "", 2); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("签名后取消仍返回结果", err)
	}
	if result, err := pages.List(ctx, actor, 12, "", 2); err != nil || result == nil || result.NextCursor == nil {
		t.Fatal("失败后无法继续读取", err)
	}

	// 5. 最大标识及最大数据库时间通过整个客户端游标分页流程。
	maximum := ^uint64(0)
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大分页用户','fixture_hash')", maximum); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-2, maximum); err != nil {
		t.Fatal(err)
	}
	maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	for index, entryID := range []uint64{maximum - 1, maximum} {
		name := []string{"最大子项一", "最大子项二"}[index]
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,?,?,?,?,'DIR',?)", entryID, maximum, maximum-2, name, []byte(name), maximumTime); err != nil {
			t.Fatal(err)
		}
	}
	identity := domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
	largeFirst, err := pages.List(ctx, identity, maximum-2, "", 1)
	if err != nil || largeFirst == nil || len(largeFirst.Entries) != 1 || largeFirst.Entries[0].ID != maximum || largeFirst.NextCursor == nil {
		t.Fatal("最大整数首页错误", err)
	}
	largeLast, err := pages.List(ctx, identity, maximum-2, *largeFirst.NextCursor, 1)
	if err != nil || largeLast == nil || len(largeLast.Entries) != 1 || largeLast.Entries[0].ID != maximum-1 || largeLast.NextCursor != nil || !largeLast.Entries[0].ModifiedAt.Equal(maximumTime) {
		t.Fatal("最大整数和时间的末页错误", err)
	}
}
