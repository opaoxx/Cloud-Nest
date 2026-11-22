package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// fileReuseSaveProbe 可在真实幂等记录写入后检查尚未提交的事务。
type fileReuseSaveProbe struct {
	*Store
	afterSave func(context.Context, *Store, *domain.IdempotencyRecord) error
}

func (probe *fileReuseSaveProbe) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	record, err := probe.Store.CreateUserIdempotencyRecord(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := probe.afterSave(ctx, probe.Store, record); err != nil {
		return nil, err
	}
	return record, nil
}

func TestIdempotentFileReuseIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	const parentID, objectID, sizeBytes = 12, 41, 2048
	const requestedName, key = "有效.md", "reuse-first-result"
	digest := sha256.Sum256(make([]byte, sizeBytes))

	// 1. 使用正式独占库，准备同账号的活动引用、已验证对象和空间账目。
	if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET status='READY',verified_sha256=?,verified_at=UTC_TIMESTAMP(6) WHERE id=?",
		digest[:], objectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=?,used_storage=?,reserved_storage=? WHERE id=?",
		3*sizeBytes+32, 2*sizeBytes, 32, actor.UserID); err != nil {
		t.Fatal(err)
	}

	type persistedState struct {
		entries, records int
		used, reserved   uint64
	}
	readState := func() persistedState {
		t.Helper()
		var state persistedState
		err := pool.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM file_entries),(SELECT COUNT(*) FROM idempotency_records),
			used_storage,reserved_storage FROM users WHERE id=?`, actor.UserID).Scan(&state.entries, &state.records, &state.used, &state.reserved)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := readState()
	encode := func(entry *domain.FileEntry) (json.RawMessage, error) {
		data, err := dto.NewReuseFileData(entry)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	validate := func(body json.RawMessage) error {
		_, err := dto.RestoreReuseFileData(body)
		return err
	}
	creator := func(afterSave func(context.Context, *Store, *domain.IdempotencyRecord) error) *service.IdempotentFileReuseService {
		return service.NewIdempotentFileReuseService(func(ctx context.Context, identity domain.Actor, action func(context.Context, service.IdempotentFileReuseTransaction) error) error {
			return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
				if afterSave != nil {
					return action(txContext, &fileReuseSaveProbe{Store: txStore, afterSave: afterSave})
				}
				return action(txContext, txStore)
			})
		}, encode, validate)
	}

	// 2. 读到三个待提交结果后模拟保存失败，验证外层全部回滚。
	saveFailure := errors.New("合成保存结果失败")
	broken := creator(func(txContext context.Context, txStore *Store, record *domain.IdempotencyRecord) error {
		content, err := txStore.FindUserFileEntryContent(txContext, actor.UserID, *record.ResourceID)
		if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != objectID {
			t.Fatalf("保存幂等记录时未找到新条目，得到 %+v 和 %v", content, err)
		}
		var used uint64
		if err := txStore.database.WithContext(txContext).Raw("SELECT used_storage FROM users WHERE id=?", actor.UserID).Row().Scan(&used); err != nil {
			return err
		}
		if used != 3*sizeBytes {
			t.Fatalf("事务内已用空间未结算，得到 %d", used)
		}
		return saveFailure
	})
	body, err := broken.Reuse(ctx, actor, parentID, requestedName, sizeBytes, digest, key)
	if !errors.Is(err, saveFailure) || body != nil || readState() != before {
		t.Fatal("保存失败后仍留下结果、文件或用量", err)
	}

	// 3. 使用同一键重新执行，验证首次成功快照与业务数据一起提交。
	reuse := creator(nil)
	body, err = reuse.Reuse(ctx, actor, parentID, requestedName, sizeBytes, digest, key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := dto.RestoreReuseFileData(body)
	if err != nil || result == nil || result.Entry.Name != "有效 (1).md" || result.Entry.ParentID != "12" || result.Entry.SizeBytes != sizeBytes {
		t.Fatalf("恢复已交付快照失败，得到 %+v 和 %v", result, err)
	}
	requestHash, err := service.BuildFileReuseRequestHash(parentID, requestedName, sizeBytes, digest)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.FileReuseOperation, key)
	if err != nil || record == nil || record.ResourceID == nil || strconv.FormatUint(*record.ResourceID, 10) != result.Entry.ID ||
		record.RequestHash != requestHash || record.ResponseStatus != 201 || record.ResourceType != "ENTRY" ||
		!record.ExpiresAt.Equal(result.Entry.CreatedAt.Add(8*24*time.Hour)) {
		t.Fatalf("保存的幂等记录不符，得到 %+v 和 %v", record, err)
	}
	saved, err := dto.RestoreReuseFileData(record.ResponseBody)
	if err != nil || saved == nil || *saved != *result {
		t.Fatalf("写入后读出的快照与交付结果不同，得到 %+v 和 %v", saved, err)
	}
	expected := before
	expected.entries++
	expected.records++
	expected.used += sizeBytes
	if readState() != expected {
		t.Fatal("成功提交未留下三项结果，或改变了已有预占")
	}

	// 4. 使用正式改名修改当前条目，同键重试继续返回旧名称和初始版本。
	const changedName = "稍后改名.md"
	metadata, err := service.BuildEntryNameMetadata("FILE", changedName)
	if err != nil {
		t.Fatal(err)
	}
	nameKey, err := service.BuildEntryNameKey(changedName)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.RenameUserFileEntry(txContext, *record.ResourceID, 1, *metadata, nameKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body, err = reuse.Reuse(ctx, actor, parentID, "  "+requestedName+"  ", sizeBytes, digest, key)
	replayed, restoreError := dto.RestoreReuseFileData(body)
	if err != nil || restoreError != nil || replayed == nil || *replayed != *result || readState() != expected {
		t.Fatalf("同键重放未交付创建快照，得到 %+v 和错误 %v、%v", replayed, err, restoreError)
	}
	live, err := store.FindUserFileEntry(ctx, actor.UserID, *record.ResourceID)
	if err != nil || live == nil || live.Name != changedName || live.Revision != 2 {
		t.Fatalf("重放改变了当前文件事实，得到 %+v 和 %v", live, err)
	}

	// 5. 参数冲突、快照关联异常和到期都不会回到首次创建。
	assertRejected := func(name string, wantError error) {
		t.Helper()
		body, err := reuse.Reuse(ctx, actor, parentID, name, sizeBytes, digest, key)
		if !errors.Is(err, wantError) || body != nil || readState() != expected {
			t.Fatalf("拒绝重放时得到正文 %s 和错误 %v，期望 %v", body, err, wantError)
		}
	}
	assertRejected("不同请求.md", domain.ErrIdempotencyRequestConflict)
	if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.entry.parent_id','11') WHERE id=?", record.ID); err != nil {
		t.Fatal(err)
	}
	assertRejected(requestedName, domain.ErrInvalidIdempotencyRecordData)
	if _, err := pool.ExecContext(ctx, `UPDATE idempotency_records SET response_body=?, created_at=UTC_TIMESTAMP(6)-INTERVAL 9 DAY,
		expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE id=?`, string(record.ResponseBody), record.ID); err != nil {
		t.Fatal(err)
	}
	assertRejected(requestedName, domain.ErrIdempotencyRecordExpired)
	assertRejected("不同请求.md", domain.ErrIdempotencyRecordExpired)
	t.Log("秒传同键重放返回改名前快照，冲突、过期和快照异常均无重复写入")
}
