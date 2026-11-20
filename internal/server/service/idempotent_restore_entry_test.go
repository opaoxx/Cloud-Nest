package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

type idempotentRestoreTransactionStub struct {
	*restoreEntryTransactionStub
	records      *idempotentDirectoryTransactionStub
	writeContext context.Context
	writeCalls   int
}

func (store *idempotentRestoreTransactionStub) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	return store.records.FindUserIdempotencyRecord(ctx, userID, operation, key)
}

func (store *idempotentRestoreTransactionStub) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	return store.records.CreateUserIdempotencyRecord(ctx, input)
}

func (store *idempotentRestoreTransactionStub) RestoreUserFileEntry(ctx context.Context, entryID, revision, parentID uint64, name string, key []byte) (*domain.FileEntry, error) {
	store.writeCalls++
	store.writeContext = ctx
	return store.restoreEntryTransactionStub.RestoreUserFileEntry(ctx, entryID, revision, parentID, name, key)
}

func idempotentRestoreFixture() (domain.Actor, *idempotentRestoreTransactionStub, *IdempotentRestoreEntryService) {
	actor, core := restoreEntryServiceFixture()
	transaction := &idempotentRestoreTransactionStub{
		restoreEntryTransactionStub: core,
		records:                     &idempotentDirectoryTransactionStub{findError: domain.ErrIdempotencyRecordNotFound},
	}
	restore := NewIdempotentRestoreEntryService(
		func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentRestoreEntryTransaction) error) error {
			return action(ctx, transaction)
		},
		func(entry *domain.FileEntry) (json.RawMessage, error) {
			data, err := dto.NewRestoreEntryData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.DecodeRestoreEntryData(body)
			return err
		},
	)
	restore.now = func() time.Time { return time.Date(2026, 11, 20, 4, 30, 0, 123456789, time.UTC) }
	return actor, transaction, restore
}

func TestIdempotentRestoreEntryServiceFirstAndReplay(t *testing.T) {
	actor, store, restore := idempotentRestoreFixture()
	store.entries[12].Status = "TRASHED"
	store.nameTaken = true
	ctx := t.Context()
	txContext, cancel := context.WithCancel(ctx)
	defer cancel()
	transactions := 0
	restore.withTransaction = func(received context.Context, identity domain.Actor, action func(context.Context, IdempotentRestoreEntryTransaction) error) error {
		if received != ctx || identity != actor {
			t.Fatal("事务入口收到错误的身份或上下文")
		}
		transactions++
		return action(txContext, store)
	}

	// 1. 首次查记录、写目录项与保存快照使用同一份事务。
	const key = "restore-007"
	first, err := restore.Restore(ctx, actor, 13, 3, key)
	if err != nil || first == nil || transactions != 1 ||
		store.writeCalls != 1 || store.records.findCalls != 1 || store.records.saveCalls != 1 {
		t.Fatal("首次恢复未在一笔事务中完成", err)
	}
	input := store.records.saveInput
	hash, err := BuildRestoreEntryRequestHash(13, 3)
	if err != nil || input.Operation != domain.RestoreEntryOperation || input.IdempotencyKey != key || input.RequestHash != hash ||
		input.ResourceType != "ENTRY" || input.ResourceID == nil || *input.ResourceID != 13 || input.ResponseStatus != 200 ||
		!input.ExpiresAt.Equal(restore.now().UTC().Truncate(time.Microsecond).Add(8*24*time.Hour)) ||
		store.records.findContext != txContext || store.records.saveContext != txContext || store.writeContext != txContext ||
		store.records.userID != actor.UserID || store.records.operation != domain.RestoreEntryOperation || store.records.key != key {
		t.Fatal("记录范围、期限或事务上下文不符", err)
	}
	data, err := dto.DecodeRestoreEntryData(first)
	if err != nil || data == nil || data.Entry.ID != "13" || data.Entry.ParentID != "11" ||
		data.Entry.Name != "学习笔记 (1).md" || data.Entry.Revision != "4" || data.Entry.Status != "ACTIVE" {
		t.Fatal("首次快照不符合实际恢复结果", data, err)
	}

	// 2. 匹配记录直接复用并复制正文，当前目录项无需读取。
	store.records.record, store.records.findError = store.records.saved, nil
	coreCalls := len(store.calls)
	store.entries = nil
	replayed, err := restore.Restore(ctx, actor, 13, 3, key)
	if err != nil || !bytes.Equal(first, replayed) || transactions != 2 ||
		store.writeCalls != 1 || store.records.saveCalls != 1 || len(store.calls) != coreCalls {
		t.Fatal("重复请求没有直接复用快照", err)
	}
	original := bytes.Clone(store.records.saved.ResponseBody)
	first[0], replayed[0] = 'x', 'y'
	if !bytes.Equal(store.records.saved.ResponseBody, original) {
		t.Fatal("修改交付正文影响了保存数据")
	}
}

func TestIdempotentRestoreEntryServiceFailures(t *testing.T) {
	failure := errors.New("fixture failure")
	for _, scenario := range []struct {
		name          string
		want          error
		writes, saves int
	}{
		{"read failure", failure, 0, 0},
		{"nil found record", domain.ErrInvalidIdempotencyRecordData, 0, 0},
		{"target no longer trashed", domain.ErrFileEntryNotFound, 0, 0},
		{"encode failure", failure, 1, 0},
		{"unusable encoded body", domain.ErrInvalidIdempotencyRecordData, 1, 0},
		{"save failure", failure, 1, 1},
		{"wrong readback version", domain.ErrInvalidIdempotencyRecordData, 1, 1},
		{"invalid readback snapshot", domain.ErrInvalidIdempotencyRecordData, 1, 1},
		{"cancellation after save", context.Canceled, 1, 1},
		{"commit failure", failure, 1, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, store, restore := idempotentRestoreFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario.name {
			case "read failure":
				store.records.findError = failure
			case "nil found record":
				store.records.findError = nil
			case "target no longer trashed":
				store.entries[13].Status = "ACTIVE"
			case "encode failure":
				restore.encodeResult = func(*domain.FileEntry) (json.RawMessage, error) { return nil, failure }
			case "unusable encoded body":
				restore.encodeResult = func(*domain.FileEntry) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
			case "save failure":
				store.records.saveError = failure
			case "wrong readback version":
				store.records.afterSave = func(record *domain.IdempotencyRecord) {
					record.ResponseBody = bytes.Replace(record.ResponseBody, []byte(`"revision":"4"`), []byte(`"revision":"5"`), 1)
				}
			case "invalid readback snapshot":
				store.records.afterSave = func(record *domain.IdempotencyRecord) {
					record.ResponseBody = bytes.Replace(record.ResponseBody, []byte(`"ACTIVE"`), []byte(`"TRASHED"`), 1)
				}
			case "cancellation after save":
				store.records.afterSave = func(*domain.IdempotencyRecord) { cancel() }
			case "commit failure":
				restore.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentRestoreEntryTransaction) error) error {
					if err := action(ctx, store); err != nil {
						return err
					}
					return failure
				}
			}
			body, err := restore.Restore(ctx, actor, 13, 3, "restore-007")
			if body != nil || !errors.Is(err, scenario.want) || store.writeCalls != scenario.writes || store.records.saveCalls != scenario.saves {
				t.Fatalf("错误 %v，尝试写入 %d、保存 %d，正文 %s", err, store.writeCalls, store.records.saveCalls, body)
			}
		})
	}
}
