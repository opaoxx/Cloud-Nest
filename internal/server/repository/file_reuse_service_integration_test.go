package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// fileReuseSettlementProbe 在正式结算前运行夹具回调，随后调用原仓储。
type fileReuseSettlementProbe struct {
	*Store
	beforeSettle func(context.Context, *Store) error
}

func (probe *fileReuseSettlementProbe) SettleUserFileReuseQuota(ctx context.Context, sizeBytes uint64) error {
	if err := probe.beforeSettle(ctx, probe.Store); err != nil {
		return err
	}
	return probe.Store.SettleUserFileReuseQuota(ctx, sizeBytes)
}

func TestFileReuseServiceIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	actor := domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
	const parentID, originalID, objectID = 13, 31, 29
	const originalName, newName = "发布笔记.MD", "发布笔记 (1).MD"
	const sizeBytes, initialUsed, initialReserved = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes, uint64(32)
	const quotaBytes = initialUsed + initialReserved + sizeBytes
	var digest [32]byte

	// 1. 复用已有活动文件，设置恰好足够且含其他上传预占的配额。
	originalKey, err := service.BuildEntryNameKey(originalName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name_key=? WHERE id=?", originalKey, originalID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=?,used_storage=?,reserved_storage=? WHERE id=?",
		quotaBytes, initialUsed, initialReserved, actor.UserID); err != nil {
		t.Fatal(err)
	}

	type databaseState struct {
		entries, objects, tasks int
		used, reserved          uint64
	}
	readState := func() databaseState {
		t.Helper()
		var state databaseState
		err := pool.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM file_entries),(SELECT COUNT(*) FROM stored_objects),
			(SELECT COUNT(*) FROM upload_tasks),used_storage,reserved_storage FROM users WHERE id=?`, actor.UserID).
			Scan(&state.entries, &state.objects, &state.tasks, &state.used, &state.reserved)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := readState()
	expectedCommit := before
	expectedCommit.entries++
	expectedCommit.used += sizeBytes
	newKey, err := service.BuildEntryNameKey(newName)
	if err != nil {
		t.Fatal(err)
	}
	assertRejected := func(entry *domain.FileEntry, err, wantError error) {
		t.Helper()
		if entry != nil || !errors.Is(err, wantError) || readState() != before {
			t.Fatalf("失败请求留下结果或改变状态，得到 %+v 和 %v，期望 %v", entry, err, wantError)
		}
	}
	newCreator := func(
		beforeSettle, beforeCommit func(context.Context, *Store) error,
	) *service.FileReuseService {
		return service.NewFileReuseService(func(requestContext context.Context, identity domain.Actor, action func(context.Context, service.FileReuseTransaction) error) error {
			return store.WithUserTransaction(requestContext, identity, func(txContext context.Context, txStore *Store) error {
				var txn service.FileReuseTransaction = txStore
				if beforeSettle != nil {
					txn = &fileReuseSettlementProbe{Store: txStore, beforeSettle: beforeSettle}
				}
				if err := action(txContext, txn); err != nil {
					return err
				}
				if beforeCommit != nil {
					return beforeCommit(txContext, txStore)
				}
				return nil
			})
		})
	}

	// 2. 未命中或目标不可见时，不创建条目也不扣账。
	creator := newCreator(nil, nil)
	wrongDigest := digest
	wrongDigest[0] = 1
	entry, err := creator.Reuse(ctx, actor, parentID, originalName, sizeBytes, wrongDigest)
	assertRejected(entry, err, domain.ErrReusableContentNotFound)
	entry, err = creator.Reuse(ctx, actor, parentID, originalName, sizeBytes-1, digest)
	assertRejected(entry, err, domain.ErrReusableContentNotFound)
	entry, err = creator.Reuse(ctx, actor, 14, originalName, sizeBytes, digest)
	assertRejected(entry, err, domain.ErrFileEntryNotFound)

	// 3. 确认已插入条目后模拟预占增加 1 B，正式结算超限，两项写入均回滚。
	probed := false
	blocked := newCreator(func(txContext context.Context, txStore *Store) error {
		pending, err := txStore.FindUserActiveFileEntryByName(txContext, actor.UserID, parentID, newKey)
		if err != nil || pending == nil || pending.FileSize != sizeBytes {
			t.Fatalf("结算前未找到刚写入的文件，得到 %+v 和 %v", pending, err)
		}
		probed = true
		return txStore.ReserveUserUploadSpace(txContext, 1)
	}, nil)
	entry, err = blocked.Reuse(ctx, actor, parentID, originalName, sizeBytes, digest)
	assertRejected(entry, err, domain.ErrQuotaExceeded)
	if !probed {
		t.Fatal("超限场景未到达正式结算")
	}

	// 4. 模拟外层事务在回调完成后失败，确认已插入且已扣账的数据一起回滚。
	commitFailure := errors.New("模拟外层提交前失败")
	rolledBack := newCreator(nil, func(txContext context.Context, txStore *Store) error {
		var pendingUsed uint64
		if err := txStore.database.WithContext(txContext).Raw("SELECT used_storage FROM users WHERE id=?", actor.UserID).Row().Scan(&pendingUsed); err != nil {
			return err
		}
		pending, err := txStore.FindUserActiveFileEntryByName(txContext, actor.UserID, parentID, newKey)
		if err != nil || pending == nil || pendingUsed != expectedCommit.used {
			t.Fatalf("事务内未看到两个结果，条目 %+v，已用 %d，错误 %v", pending, pendingUsed, err)
		}
		return commitFailure
	})
	entry, err = rolledBack.Reuse(ctx, actor, parentID, originalName, sizeBytes, digest)
	assertRejected(entry, err, commitFailure)

	// 5. 同一份请求在正常提交后返回新条目，并按完整大小增加已用空间。
	entry, err = creator.Reuse(ctx, actor, parentID, originalName, sizeBytes, digest)
	if err != nil || entry == nil || entry.ID == originalID || entry.Name != newName || entry.FileSize != sizeBytes || readState() != expectedCommit {
		t.Fatalf("秒传提交未同时保存文件和账目，得到 %+v 和 %v", entry, err)
	}
	content, err := store.FindUserFileEntryContent(ctx, actor.UserID, entry.ID)
	if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != objectID {
		t.Fatalf("新目录项未引用原对象，得到 %+v 和 %v", content, err)
	}
	t.Log("同一事务提交目录项及完整逻辑用量，结算和外层异常均回滚")
}
