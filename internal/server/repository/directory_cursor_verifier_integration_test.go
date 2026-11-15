package repository

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestDirectoryCursorVerifierIntegration(t *testing.T) {
	// 1. 在正式表独占库中装配签名、校验和目录列表服务。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','游标校验用户','fixture_hash')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(12,1,11,'课程资料',CAST('课程资料' AS BINARY),'DIR'),(14,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR'),(21,1,12,'子项一',CAST('子项一' AS BINARY),'DIR'),(22,1,12,'子项二',CAST('子项二' AS BINARY),'DIR'),(23,1,12,'子项三',CAST('子项三' AS BINARY),'DIR')",
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
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
	first, err := lists.List(ctx, actor, 12, nil, 2)
	if err != nil || first == nil || len(first.Entries) != 2 || !first.HasMore || first.Entries[1].ID != 22 {
		t.Fatal("真实目录首页错误", err)
	}
	last := first.Entries[1]
	cursor, err := signer.Sign(actor.UserID, 12, domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
	if err != nil {
		t.Fatal(err)
	}
	listCalls := 0
	readNext := func(userID, parentID uint64, text string) (*domain.DirectoryList, error) {
		position, err := verifier.Verify(userID, parentID, text)
		if err != nil {
			return nil, err
		}
		listCalls++
		identity := actor
		identity.UserID = userID
		return lists.List(ctx, identity, parentID, position, 2)
	}

	// 2. 实际校验器还原同时间页末项，错误输入在目录列表调用前结束。
	next, err := readNext(1, 12, cursor)
	if err != nil || next == nil || len(next.Entries) != 1 || next.HasMore || next.Entries[0].ID != 21 || len(next.Path.Entries) != 2 {
		t.Fatal("校验后的真实后续页错误", err)
	}
	for _, testCase := range []struct {
		userID, parentID uint64
		cursor           string
	}{
		{3, 12, cursor}, {1, 14, cursor}, {1, 12, cursor + ".extra"}, {1, 12, "invalid-cursor"},
	} {
		beforeCalls := listCalls
		if result, err := readNext(testCase.userID, testCase.parentID, testCase.cursor); result != nil || !errors.Is(err, domain.ErrInvalidDirectoryCursor) || listCalls != beforeCalls {
			t.Fatal("无效游标仍进入目录列表服务", err)
		}
	}
	recreated, err := service.NewDirectoryCursorVerifier(bytes.Clone(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	if position, err := recreated.Verify(1, 12, cursor); err != nil || position == nil || position.ID != 22 || !position.ModifiedAt.Equal(stamp) {
		t.Fatal("持久主密钥重建后未还原原位置", err)
	}

	// 3. 签名通过后继续检查目录的当前可见性。
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
		t.Fatal(err)
	}
	if result, err := readNext(1, 12, cursor); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
		t.Fatal("签名正确但已回收目录仍可读取", err)
	}

	// 4. 最后建立完整无符号主键及最大微秒时间，实际衔接下一页。
	maximum := ^uint64(0)
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-2, maximum); err != nil {
		t.Fatal(err)
	}
	maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	for index, entryID := range []uint64{maximum - 1, maximum} {
		name := []string{"末项一", "末项二"}[index]
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,?,?,?,?,'DIR',?)", entryID, maximum, maximum-2, name, []byte(name), maximumTime); err != nil {
			t.Fatal(err)
		}
	}
	identity := domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
	largePage, err := lists.List(ctx, identity, maximum-2, nil, 1)
	if err != nil || largePage == nil || len(largePage.Entries) != 1 || !largePage.HasMore || largePage.Entries[0].ID != maximum {
		t.Fatal("最大标识首页错误", err)
	}
	largeCursor, err := signer.Sign(maximum, maximum-2, domain.FileEntryPosition{ModifiedAt: largePage.Entries[0].ModifiedAt, ID: maximum})
	if err != nil {
		t.Fatal(err)
	}
	position, err := verifier.Verify(maximum, maximum-2, largeCursor)
	if err != nil || position == nil || position.ID != maximum || !position.ModifiedAt.Equal(maximumTime) {
		t.Fatal("最大标识或时间校验丢失", err)
	}
	largeNext, err := lists.List(ctx, identity, maximum-2, position, 1)
	if err != nil || largeNext == nil || len(largeNext.Entries) != 1 || largeNext.HasMore || largeNext.Entries[0].ID != maximum-1 {
		t.Fatal("完整整数校验位置未衔接末页", err)
	}
}
