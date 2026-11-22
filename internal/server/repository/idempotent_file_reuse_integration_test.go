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

func TestIdempotentFileReuseFirstResultIntegration(t *testing.T) {
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

	// 4. 已有同键记录时结束首次路径，保留之前提交的数据。
	body, err = reuse.Reuse(ctx, actor, parentID, requestedName, sizeBytes, digest, key)
	if !errors.Is(err, domain.ErrIdempotencyRecordAlreadyExists) || body != nil || readState() != expected {
		t.Fatal("已占用的键再次走首次路径或改变了数据", err)
	}
	t.Log("条目、已用空间和 201 快照同事务提交，保存错误时三者同时回滚")
}
