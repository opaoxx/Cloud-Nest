package service

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type trashEntryTransactionStub struct {
	*fileEntryPathReaderStub
	saved        *domain.TrashedFileEntry
	writeError   error
	writeCalls   int
	writeContext context.Context
	entryID      uint64
	revision     uint64
	afterWrite   func()
}

func (transaction *trashEntryTransactionStub) TrashUserFileEntry(ctx context.Context, entryID, expectedRevision uint64) (*domain.TrashedFileEntry, error) {
	transaction.writeCalls++
	transaction.writeContext, transaction.entryID, transaction.revision = ctx, entryID, expectedRevision
	if transaction.afterWrite != nil {
		transaction.afterWrite()
	}
	return transaction.saved, transaction.writeError
}

func trashEntryServiceFixture() (domain.Actor, *trashEntryTransactionStub) {
	actor, reader := fileEntryPathFixture()
	entry := *reader.entries[13]
	entry.ParentID = copyOptionalValue(entry.ParentID)
	entry.Status, entry.Revision = "TRASHED", entry.Revision+1
	deletedAt := entry.ModifiedAt.Add(time.Minute)
	entry.ModifiedAt, entry.UpdatedAt = deletedAt, deletedAt
	return actor, &trashEntryTransactionStub{
		fileEntryPathReaderStub: reader,
		saved:                   &domain.TrashedFileEntry{FileEntry: entry, DeletedAt: deletedAt},
	}
}

func TestTrashEntryServiceResults(t *testing.T) {
	for _, scenario := range []string{"file", "directory", "maximum increment", "UTC conversion"} {
		t.Run(scenario, func(t *testing.T) {
			actor, transaction := trashEntryServiceFixture()
			target := transaction.entries[13]
			switch scenario {
			case "directory":
				target.EntryType, target.FileSize = "DIR", 0
				transaction.saved.EntryType, transaction.saved.FileSize = "DIR", 0
			case "maximum increment":
				target.Revision, transaction.saved.Revision = math.MaxUint64-1, math.MaxUint64
			case "UTC conversion":
				transaction.saved.DeletedAt = transaction.saved.DeletedAt.In(time.FixedZone("UTC+08", 8*3600))
			}
			want := *transaction.saved
			want.ParentID = copyOptionalValue(want.ParentID)
			want.DeletedAt = want.DeletedAt.UTC()
			type txContextKey struct{}
			txContext := context.WithValue(t.Context(), txContextKey{}, "trash")
			calls := 0
			trash := NewTrashEntryService(func(ctx context.Context, received domain.Actor, action func(context.Context, TrashEntryTransaction) error) error {
				calls++
				if ctx != t.Context() || received != actor {
					t.Error("事务调用上下文或身份不符")
				}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				// 提交前修改仓储测试结果，核对服务已准备独立副本。
				*transaction.saved.ParentID = 99
				transaction.saved.Name = "later change"
				return nil
			})

			// 1. 核对读取和写入共享同一事务上下文，提交后交付完整独立结果。
			result, err := trash.Trash(t.Context(), actor, target.ID, target.Revision)
			if err != nil || !reflect.DeepEqual(result, &want) || calls != 1 || transaction.writeCalls != 1 ||
				transaction.writeContext != txContext || transaction.entryID != target.ID || transaction.revision != target.Revision ||
				!reflect.DeepEqual(transaction.entryIDs, []uint64{13, 12, 11}) {
				t.Fatal("回收事务调用或结果不符", result, err)
			}
			for index, readContext := range transaction.contexts {
				if readContext != txContext || transaction.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未使用同一事务和用户范围")
				}
			}
		})
	}
}

func TestTrashEntryServiceRejections(t *testing.T) {
	lockedConflict := &domain.EntryRevisionConflictError{CurrentRevision: 4}
	writeError := errors.New("fixture write failed")
	for _, scenario := range []struct {
		name   string
		edit   func(*trashEntryTransactionStub)
		want   error
		writes int
	}{
		{"hidden ancestor", func(tx *trashEntryTransactionStub) { tx.entries[12].Status = "TRASHED" }, domain.ErrFileEntryNotFound, 0},
		{"stale version", func(tx *trashEntryTransactionStub) { tx.entries[13].Revision = 4 }, domain.ErrEntryRevisionConflict, 0},
		{"locked version conflict", func(tx *trashEntryTransactionStub) { tx.writeError = lockedConflict }, lockedConflict, 1},
		{"write failure", func(tx *trashEntryTransactionStub) { tx.writeError = writeError }, writeError, 1},
		{"nil result", func(tx *trashEntryTransactionStub) { tx.saved = nil }, domain.ErrInvalidFileEntryData, 1},
		{"wrong target", func(tx *trashEntryTransactionStub) { tx.saved.ID++ }, domain.ErrInvalidFileEntryData, 1},
		{"wrong owner", func(tx *trashEntryTransactionStub) { tx.saved.UserID++ }, domain.ErrInvalidFileEntryData, 1},
		{"nil parent", func(tx *trashEntryTransactionStub) { tx.saved.ParentID = nil }, domain.ErrInvalidFileEntryData, 1},
		{"changed parent", func(tx *trashEntryTransactionStub) { *tx.saved.ParentID = 99 }, domain.ErrInvalidFileEntryData, 1},
		{"changed name", func(tx *trashEntryTransactionStub) { tx.saved.Name = "other" }, domain.ErrInvalidFileEntryData, 1},
		{"changed type", func(tx *trashEntryTransactionStub) { tx.saved.EntryType = "DIR" }, domain.ErrInvalidFileEntryData, 1},
		{"changed size", func(tx *trashEntryTransactionStub) { tx.saved.FileSize++ }, domain.ErrInvalidFileEntryData, 1},
		{"changed creation time", func(tx *trashEntryTransactionStub) { tx.saved.CreatedAt = tx.saved.CreatedAt.Add(time.Second) }, domain.ErrInvalidFileEntryData, 1},
		{"wrong status", func(tx *trashEntryTransactionStub) { tx.saved.Status = "ACTIVE" }, domain.ErrInvalidFileEntryData, 1},
		{"wrong revision", func(tx *trashEntryTransactionStub) { tx.saved.Revision++ }, domain.ErrInvalidFileEntryData, 1},
		{"invalid deletion time", func(tx *trashEntryTransactionStub) { tx.saved.DeletedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"excess time precision", func(tx *trashEntryTransactionStub) { tx.saved.DeletedAt = tx.saved.DeletedAt.Add(time.Nanosecond) }, domain.ErrInvalidFileEntryData, 1},
		{"mismatched modified time", func(tx *trashEntryTransactionStub) { tx.saved.ModifiedAt = tx.saved.DeletedAt.Add(time.Second) }, domain.ErrInvalidFileEntryData, 1},
		{"mismatched updated time", func(tx *trashEntryTransactionStub) { tx.saved.UpdatedAt = tx.saved.DeletedAt.Add(time.Second) }, domain.ErrInvalidFileEntryData, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, transaction := trashEntryServiceFixture()
			scenario.edit(transaction)
			trash := NewTrashEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, TrashEntryTransaction) error) error {
				return action(ctx, transaction)
			})
			// 1. 前置检查拒绝时不进入写入，写后异常交给外层回滚。
			result, err := trash.Trash(t.Context(), actor, 13, 3)
			if result != nil || !errors.Is(err, scenario.want) || transaction.writeCalls != scenario.writes {
				t.Fatal("回收错误链或写入次数不符", result, err, transaction.writeCalls)
			}
		})
	}
}

func TestTrashEntryServiceTransactionBoundary(t *testing.T) {
	commitError := errors.New("fixture commit failed")
	for _, mode := range []string{
		"invalid actor", "zero revision", "canceled input", "nil transaction context", "nil transaction store",
		"canceled write", "commit failure", "no callback", "canceled after commit",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := trashEntryServiceFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			revision := uint64(3)
			expectedTransactions, expectedWrites := 1, 0
			var expectedError error
			switch mode {
			case "invalid actor":
				actor.SessionID, expectedTransactions = 0, 0
			case "zero revision":
				revision, expectedTransactions = 0, 0
			case "canceled input":
				cancel()
				expectedError, expectedTransactions = context.Canceled, 0
			case "canceled write":
				transaction.afterWrite = cancel
				expectedError, expectedWrites = context.Canceled, 1
			case "commit failure":
				expectedError, expectedWrites = commitError, 1
			case "no callback":
				expectedError = domain.ErrInvalidFileEntryData
			case "canceled after commit":
				expectedWrites = 1
			}
			calls := 0
			trash := NewTrashEntryService(func(txContext context.Context, _ domain.Actor, action func(context.Context, TrashEntryTransaction) error) error {
				calls++
				switch mode {
				case "nil transaction context":
					return action(nil, transaction)
				case "nil transaction store":
					return action(txContext, nil)
				case "no callback":
					return nil
				}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				if mode == "commit failure" {
					return commitError
				}
				cancel()
				return nil
			})
			// 1. 回调完成与事务提交分别判断，已确认提交的结果仍可交付。
			result, err := trash.Trash(ctx, actor, 13, revision)
			if mode == "canceled after commit" {
				if result == nil || err != nil {
					t.Fatal("确认提交的结果被丢弃", result, err)
				}
			} else if result != nil || err == nil || (expectedError != nil && !errors.Is(err, expectedError)) {
				t.Fatal("未确认提交却交付了结果，或错误不符", result, err)
			}
			if calls != expectedTransactions || transaction.writeCalls != expectedWrites {
				t.Fatal("事务或写入调用次数不符", calls, transaction.writeCalls)
			}
		})
	}
}

func TestNewTrashEntryServiceRequiresTransaction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少事务装配时未拒绝构造")
		}
	}()
	NewTrashEntryService(nil)
}
