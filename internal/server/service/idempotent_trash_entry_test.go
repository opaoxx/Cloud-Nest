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

type idempotentTrashTransactionStub struct {
	*trashEntryTransactionStub
	records *idempotentDirectoryTransactionStub
}

func (transaction *idempotentTrashTransactionStub) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	return transaction.records.FindUserIdempotencyRecord(ctx, userID, operation, key)
}

func (transaction *idempotentTrashTransactionStub) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	return transaction.records.CreateUserIdempotencyRecord(ctx, input)
}

func idempotentTrashFixture() (domain.Actor, *idempotentTrashTransactionStub, *IdempotentTrashEntryService) {
	actor, core := trashEntryServiceFixture()
	transaction := &idempotentTrashTransactionStub{
		trashEntryTransactionStub: core,
		records:                   &idempotentDirectoryTransactionStub{findError: domain.ErrIdempotencyRecordNotFound},
	}
	trash := NewIdempotentTrashEntryService(
		func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentTrashEntryTransaction) error) error {
			return action(ctx, transaction)
		},
		func(entry *domain.TrashedFileEntry) (json.RawMessage, error) {
			data, err := dto.NewTrashEntryData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.RestoreTrashEntryData(body)
			return err
		},
	)
	trash.now = func() time.Time { return time.Date(2026, 11, 20, 4, 30, 0, 123456789, time.UTC) }
	return actor, transaction, trash
}

func TestIdempotentTrashEntryServiceFirstAndReplay(t *testing.T) {
	actor, transaction, trash := idempotentTrashFixture()
	type txContextKey struct{}
	txContext := context.WithValue(t.Context(), txContextKey{}, "trash")
	transactionCalls := 0
	trash.withTransaction = func(ctx context.Context, received domain.Actor, action func(context.Context, IdempotentTrashEntryTransaction) error) error {
		transactionCalls++
		if ctx != t.Context() || received != actor {
			t.Error("事务入口上下文或身份不符")
		}
		return action(txContext, transaction)
	}

	// 1. 核对首次查询、回收和保存共用同一个事务及期限。
	first, err := trash.Trash(t.Context(), actor, 13, 3, "trash-007")
	if err != nil || first == nil || transactionCalls != 1 || transaction.writeCalls != 1 ||
		transaction.records.findCalls != 1 || transaction.records.saveCalls != 1 {
		t.Fatal("首次回收没有在一个事务中保存结果", err)
	}
	input := transaction.records.saveInput
	digest, err := BuildTrashEntryRequestHash(13, 3)
	if err != nil || input.Operation != domain.TrashEntryOperation || input.IdempotencyKey != "trash-007" ||
		input.ResourceType != "ENTRY" || input.ResourceID == nil || *input.ResourceID != 13 ||
		input.ResponseStatus != 200 || input.RequestHash != digest ||
		input.ExpiresAt != trash.now().UTC().Truncate(time.Microsecond).Add(8*24*time.Hour) {
		t.Fatal("保存范围、请求摘要或记录期限不符", err)
	}
	if transaction.records.findContext != txContext || transaction.records.saveContext != txContext ||
		transaction.writeContext != txContext || transaction.records.userID != actor.UserID ||
		transaction.records.operation != domain.TrashEntryOperation || transaction.records.key != "trash-007" {
		t.Fatal("读取、回收或保存未复用同一事务范围")
	}
	data, err := dto.RestoreTrashEntryData(first)
	if err != nil || data.Entry.ID != "13" || data.Entry.Revision != "4" || data.Entry.Status != "TRASHED" {
		t.Fatal("保存快照与回收结果不符", data, err)
	}

	// 2. 重试直接用已存快照，即使实时目录项不再可读。
	transaction.records.record, transaction.records.findError = transaction.records.saved, nil
	readCount := len(transaction.contexts)
	transaction.entries = nil
	replayed, err := trash.Trash(t.Context(), actor, 13, 3, "trash-007")
	if err != nil || !bytes.Equal(first, replayed) || transactionCalls != 2 || transaction.records.findCalls != 2 ||
		transaction.writeCalls != 1 || transaction.records.saveCalls != 1 || len(transaction.contexts) != readCount {
		t.Fatal("同键重试未直接复用原结果", err)
	}
	original := bytes.Clone(transaction.records.saved.ResponseBody)
	first[0], replayed[0] = 'x', 'y'
	if !bytes.Equal(transaction.records.saved.ResponseBody, original) {
		t.Fatal("交付正文共享了已保存数据")
	}
}

func TestIdempotentTrashEntryServiceFailures(t *testing.T) {
	failure := errors.New("fixture operation failed")
	for _, scenario := range []struct {
		mode          string
		want          error
		writes, saves int
	}{
		{"query failure", failure, 0, 0},
		{"nil found record", domain.ErrInvalidIdempotencyRecordData, 0, 0},
		{"missing with record", domain.ErrInvalidIdempotencyRecordData, 0, 0},
		{"hidden target", domain.ErrFileEntryNotFound, 0, 0},
		{"encode failure", failure, 1, 0},
		{"invalid encoded result", domain.ErrInvalidIdempotencyRecordData, 1, 0},
		{"save failure", failure, 1, 1},
		{"wrong readback revision", domain.ErrInvalidIdempotencyRecordData, 1, 1},
		{"bad saved result", domain.ErrInvalidIdempotencyRecordData, 0, 0},
		{"commit failure", failure, 1, 1},
		{"canceled after lookup", context.Canceled, 0, 0},
		{"canceled after save", context.Canceled, 1, 1},
		{"no callback", domain.ErrInvalidIdempotencyRecordData, 0, 0},
	} {
		t.Run(scenario.mode, func(t *testing.T) {
			actor, tx, trash := idempotentTrashFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch scenario.mode {
			case "query failure":
				tx.records.findError = failure
			case "nil found record":
				tx.records.findError = nil
			case "missing with record":
				tx.records.record, _ = trashEntryReplayFixture(t)
			case "hidden target":
				tx.entries[12].Status = "TRASHED"
			case "encode failure":
				trash.encodeResult = func(*domain.TrashedFileEntry) (json.RawMessage, error) { return nil, failure }
			case "invalid encoded result":
				trash.encodeResult = func(*domain.TrashedFileEntry) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
			case "save failure":
				tx.records.saveError = failure
			case "wrong readback revision":
				tx.records.afterSave = func(record *domain.IdempotencyRecord) {
					record.ResponseBody = bytes.Replace(record.ResponseBody, []byte(`"revision":"4"`), []byte(`"revision":"5"`), 1)
				}
			case "bad saved result":
				record, _ := trashEntryReplayFixture(t)
				record.ResourceID = new(uint64(13))
				record.IdempotencyKey = "trash-007"
				record.RequestHash, _ = BuildTrashEntryRequestHash(13, 3)
				record.ResponseBody = json.RawMessage(`{"entry":{"id":"13","revision":"4"}}`)
				tx.records.record, tx.records.findError = record, nil
			case "commit failure":
				trash.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentTrashEntryTransaction) error) error {
					if err := action(ctx, tx); err != nil {
						return err
					}
					return failure
				}
			case "canceled after lookup":
				tx.records.afterFind = cancel
			case "canceled after save":
				tx.records.afterSave = func(*domain.IdempotencyRecord) { cancel() }
			case "no callback":
				trash.withTransaction = func(context.Context, domain.Actor, func(context.Context, IdempotentTrashEntryTransaction) error) error {
					return nil
				}
			}
			body, err := trash.Trash(ctx, actor, 13, 3, "trash-007")
			if body != nil || !errors.Is(err, scenario.want) || tx.writeCalls != scenario.writes || tx.records.saveCalls != scenario.saves {
				t.Fatal("失败仍交付正文、错误不符或继续执行了写入", err, tx.writeCalls, tx.records.saveCalls)
			}
		})
	}
}
