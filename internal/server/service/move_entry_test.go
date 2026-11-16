package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type moveEntryTransactionStub struct {
	*fileEntryPathReaderStub
	stats        *domain.EntrySubtreeStats
	statsError   error
	statsCalls   int
	statsContext context.Context
	statsUserID  uint64
	statsEntryID uint64
	result       *domain.FileEntry
	writeError   error
	writeCalls   int
	writeContext context.Context
	entryID      uint64
	revision     uint64
	targetID     uint64
	afterStats   func()
	afterWrite   func()
}

func (transaction *moveEntryTransactionStub) FindUserEntrySubtreeStats(ctx context.Context, userID, entryID uint64) (*domain.EntrySubtreeStats, error) {
	transaction.statsCalls++
	transaction.statsContext, transaction.statsUserID, transaction.statsEntryID = ctx, userID, entryID
	if transaction.afterStats != nil {
		transaction.afterStats()
	}
	return transaction.stats, transaction.statsError
}

func (transaction *moveEntryTransactionStub) MoveUserFileEntry(ctx context.Context, entryID, expectedRevision, targetParentID uint64) (*domain.FileEntry, error) {
	transaction.writeCalls++
	transaction.writeContext, transaction.entryID, transaction.revision, transaction.targetID = ctx, entryID, expectedRevision, targetParentID
	if transaction.afterWrite != nil {
		transaction.afterWrite()
	}
	return transaction.result, transaction.writeError
}

func moveEntryServiceFixture() (domain.Actor, *moveEntryTransactionStub) {
	actor, reader := entryMoveTargetFixture()
	result := *reader.entries[13]
	targetID := uint64(14)
	result.ParentID, result.Revision = &targetID, result.Revision+1
	result.ModifiedAt, result.UpdatedAt = result.ModifiedAt.Add(time.Minute), result.UpdatedAt.Add(time.Minute)
	return actor, &moveEntryTransactionStub{
		fileEntryPathReaderStub: reader,
		stats:                   &domain.EntrySubtreeStats{UserID: actor.UserID, EntryID: 13, NodeCount: 1}, result: &result,
	}
}

func TestMoveEntryServiceResults(t *testing.T) {
	for _, mode := range []string{"file", "directory", "root destination", "same position", "maximum same position", "maximum increment", "maximum identifiers", "count limit"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := moveEntryServiceFixture()
			source, targetID := transaction.entries[13], uint64(14)
			switch mode {
			case "directory", "count limit":
				source.EntryType, source.FileSize = "DIR", 0
				transaction.result.EntryType, transaction.result.FileSize = "DIR", 0
				transaction.stats.NodeCount, transaction.stats.MaxRelativeDepth = 3, 1
				if mode == "count limit" {
					transaction.stats.NodeCount = 10000
				}
			case "root destination":
				targetID = 11
				transaction.result.ParentID = &targetID
			case "same position", "maximum same position":
				targetID = *source.ParentID
				if mode == "maximum same position" {
					source.Revision = ^uint64(0)
				}
				result := *source
				transaction.result = &result
			case "maximum increment":
				source.Revision, transaction.result.Revision = ^uint64(0)-1, ^uint64(0)
			case "maximum identifiers":
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
				transaction.profile.ID = maximum
				for _, entry := range transaction.entries {
					entry.UserID = maximum
				}
				destination := transaction.entries[14]
				delete(transaction.entries, 13)
				delete(transaction.entries, 14)
				source.ID, destination.ID, targetID = maximum, maximum-1, maximum-1
				transaction.entries[source.ID], transaction.entries[targetID] = source, destination
				transaction.result.ID, transaction.result.UserID, transaction.result.ParentID = maximum, maximum, &targetID
				transaction.stats.UserID, transaction.stats.EntryID = maximum, maximum
			}
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "move")
			calls := 0
			mover := NewMoveEntryService(func(ctx context.Context, current domain.Actor, action func(context.Context, MoveEntryTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务上下文或身份错误")
				}
				return action(txContext, transaction)
			})
			entry, err := mover.Move(requestContext, actor, source.ID, source.Revision, targetID)
			if err != nil || !reflect.DeepEqual(entry, transaction.result) || calls != 1 || transaction.statsCalls != 1 || transaction.writeCalls != 1 ||
				transaction.statsContext != txContext || transaction.statsUserID != actor.UserID || transaction.statsEntryID != source.ID ||
				transaction.writeContext != txContext || transaction.entryID != source.ID || transaction.revision != source.Revision || transaction.targetID != targetID {
				t.Fatal("结果、单次事务或调用参数错误", err)
			}
			for index, ctx := range transaction.contexts {
				if ctx != txContext || transaction.userIDs[index] != actor.UserID {
					t.Fatal("路径未复用事务上下文与用户")
				}
			}
			if entry == transaction.result || entry.ParentID == transaction.result.ParentID {
				t.Fatal("输出未复制")
			}
			expectedParent := *entry.ParentID
			*transaction.result.ParentID = 999
			transaction.result.Name = "后续修改"
			if *entry.ParentID != expectedParent || entry.Name == "后续修改" {
				t.Fatal("结果受到事务对象后续修改影响")
			}
		})
	}
}

func TestMoveEntryServiceFailures(t *testing.T) {
	failure := errors.New("fixture move failure")
	for _, testCase := range []struct {
		name          string
		change        func(*moveEntryTransactionStub)
		want          error
		stats, writes int
	}{
		{"missing source", func(tx *moveEntryTransactionStub) { delete(tx.entries, 13) }, domain.ErrFileEntryNotFound, 0, 0},
		{"missing destination", func(tx *moveEntryTransactionStub) { delete(tx.entries, 14) }, domain.ErrFileEntryNotFound, 0, 0},
		{"hidden source ancestor", func(tx *moveEntryTransactionStub) { tx.entries[12].Status = "TRASHED" }, domain.ErrFileEntryNotFound, 0, 0},
		{"hidden destination", func(tx *moveEntryTransactionStub) { tx.entries[14].Status = "PURGED" }, domain.ErrFileEntryNotFound, 0, 0},
		{"disabled", func(tx *moveEntryTransactionStub) { tx.profile.Status = "DISABLED" }, domain.ErrAccountDisabled, 0, 0},
		{"foreign source", func(tx *moveEntryTransactionStub) { tx.entries[13].UserID++ }, domain.ErrInvalidFileEntryData, 0, 0},
		{"query failure", func(tx *moveEntryTransactionStub) { tx.entryErrors[14] = failure }, failure, 0, 0},
		{"revision before stats", func(tx *moveEntryTransactionStub) { tx.entries[13].Revision++ }, domain.ErrEntryRevisionConflict, 0, 0},
		{"stats failure", func(tx *moveEntryTransactionStub) { tx.statsError = failure }, failure, 1, 0},
		{"nil stats", func(tx *moveEntryTransactionStub) { tx.stats = nil }, domain.ErrInvalidFileEntryData, 1, 0},
		{"foreign stats", func(tx *moveEntryTransactionStub) { tx.stats.UserID++ }, domain.ErrInvalidFileEntryData, 1, 0},
		{"wrong stats source", func(tx *moveEntryTransactionStub) { tx.stats.EntryID++ }, domain.ErrInvalidFileEntryData, 1, 0},
		{"file children", func(tx *moveEntryTransactionStub) { tx.stats.NodeCount, tx.stats.MaxRelativeDepth = 2, 1 }, domain.ErrInvalidFileEntryData, 1, 0},
		{"too many", func(tx *moveEntryTransactionStub) {
			tx.entries[13].EntryType, tx.entries[13].FileSize = "DIR", 0
			tx.stats.NodeCount, tx.stats.MaxRelativeDepth = 10001, 1
		}, domain.ErrFileEntrySubtreeTooLarge, 1, 0},
		{"too deep", func(tx *moveEntryTransactionStub) {
			tx.entries[13].EntryType, tx.entries[13].FileSize = "DIR", 0
			tx.stats.NodeCount, tx.stats.MaxRelativeDepth = 32, 31
		}, domain.ErrFileEntryDepthExceeded, 1, 0},
		{"maximum changed position", func(tx *moveEntryTransactionStub) { tx.entries[13].Revision = ^uint64(0) }, domain.ErrEntryRevisionExhausted, 1, 0},
		{"name conflict", func(tx *moveEntryTransactionStub) { tx.writeError = domain.ErrEntryNameConflict }, domain.ErrEntryNameConflict, 1, 1},
		{"locked version conflict", func(tx *moveEntryTransactionStub) {
			tx.writeError = &domain.EntryRevisionConflictError{CurrentRevision: 4}
		}, domain.ErrEntryRevisionConflict, 1, 1},
		{"write failure", func(tx *moveEntryTransactionStub) { tx.writeError = failure }, failure, 1, 1},
		{"nil result", func(tx *moveEntryTransactionStub) { tx.result = nil }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong id", func(tx *moveEntryTransactionStub) { tx.result.ID++ }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong owner", func(tx *moveEntryTransactionStub) { tx.result.UserID++ }, domain.ErrInvalidFileEntryData, 1, 1},
		{"nil parent", func(tx *moveEntryTransactionStub) { tx.result.ParentID = nil }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong parent", func(tx *moveEntryTransactionStub) { *tx.result.ParentID = 99 }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong name", func(tx *moveEntryTransactionStub) { tx.result.Name = "其他名称" }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong type", func(tx *moveEntryTransactionStub) { tx.result.EntryType = "DIR" }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong size", func(tx *moveEntryTransactionStub) { tx.result.FileSize++ }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong status", func(tx *moveEntryTransactionStub) { tx.result.Status = "TRASHED" }, domain.ErrInvalidFileEntryData, 1, 1},
		{"wrong revision", func(tx *moveEntryTransactionStub) { tx.result.Revision++ }, domain.ErrInvalidFileEntryData, 1, 1},
		{"changed creation", func(tx *moveEntryTransactionStub) { tx.result.CreatedAt = tx.result.CreatedAt.Add(time.Second) }, domain.ErrInvalidFileEntryData, 1, 1},
		{"zero creation", func(tx *moveEntryTransactionStub) { tx.result.CreatedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1, 1},
		{"zero modified", func(tx *moveEntryTransactionStub) { tx.result.ModifiedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1, 1},
		{"zero updated", func(tx *moveEntryTransactionStub) { tx.result.UpdatedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, transaction := moveEntryServiceFixture()
			testCase.change(transaction)
			revision := uint64(3)
			if testCase.name == "maximum changed position" {
				revision = ^uint64(0)
			}
			mover := NewMoveEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, MoveEntryTransaction) error) error {
				return action(ctx, transaction)
			})
			entry, err := mover.Move(context.Background(), actor, 13, revision, 14)
			if entry != nil || !errors.Is(err, testCase.want) || transaction.statsCalls != testCase.stats || transaction.writeCalls != testCase.writes {
				t.Fatal("错误链、空结果或调用次数错误", err)
			}
			if errors.Is(err, domain.ErrEntryRevisionConflict) {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != 4 {
					t.Fatal("当前版本丢失")
				}
			}
		})
	}
}

func TestMoveEntryServiceInputsAndTransaction(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero source", "zero revision", "zero destination", "canceled input", "nil tx context", "nil store", "canceled tx", "root", "file destination", "self directory", "descendant", "same position stale", "same position changed time", "same position over limit", "canceled profile", "canceled path", "canceled stats", "canceled write", "canceled before commit", "commit failure", "no callback", "canceled after commit"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := moveEntryServiceFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requestContext context.Context = ctx
			sourceID, revision, targetID := uint64(13), uint64(3), uint64(14)
			var want error
			switch mode {
			case "nil context":
				requestContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero source":
				sourceID = 0
			case "zero revision":
				revision = 0
			case "zero destination":
				targetID = 0
			case "canceled input":
				cancel()
				want = context.Canceled
			case "root":
				sourceID, revision, want = 11, 1, domain.ErrRootEntryMove
			case "file destination":
				targetID, want = 13, domain.ErrFileEntryNotDirectory
			case "self directory":
				sourceID, revision, targetID, want = 12, 1, 12, domain.ErrEntryMoveIntoSubtree
			case "descendant":
				sourceID, revision, targetID, want = 12, 1, 16, domain.ErrEntryMoveIntoSubtree
			case "same position stale":
				targetID, transaction.entries[13].Revision, want = 12, 4, domain.ErrEntryRevisionConflict
			case "same position changed time":
				targetID, transaction.result.Revision, transaction.result.ParentID, want = 12, 3, copyOptionalValue(transaction.entries[13].ParentID), domain.ErrInvalidFileEntryData
			case "same position over limit":
				targetID, want = 12, domain.ErrFileEntrySubtreeTooLarge
				transaction.entries[13].EntryType, transaction.entries[13].FileSize = "DIR", 0
				transaction.stats.NodeCount, transaction.stats.MaxRelativeDepth = 10001, 1
			case "canceled profile":
				transaction.afterProfile, want = cancel, context.Canceled
			case "canceled path":
				transaction.afterEntry, want = func(uint64) { cancel() }, context.Canceled
			case "canceled stats":
				transaction.afterStats, transaction.statsError, want = cancel, errors.New("stats failed"), context.Canceled
			case "canceled write":
				transaction.afterWrite, transaction.writeError, want = cancel, errors.New("write failed"), context.Canceled
			case "canceled tx", "canceled before commit":
				want = context.Canceled
			case "commit failure":
				want = errors.New("fixture commit failed")
			case "no callback":
				want = domain.ErrInvalidFileEntryData
			}
			calls := 0
			mover := NewMoveEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, MoveEntryTransaction) error) error {
				calls++
				switch mode {
				case "nil tx context":
					return action(nil, transaction)
				case "nil store":
					return action(ctx, nil)
				case "canceled tx":
					cancel()
				case "no callback":
					return nil
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				switch mode {
				case "canceled before commit":
					cancel()
					return ctx.Err()
				case "commit failure":
					return want
				case "canceled after commit":
					cancel()
				}
				return nil
			})
			entry, err := mover.Move(requestContext, actor, sourceID, revision, targetID)
			if mode == "canceled after commit" {
				if err != nil || entry == nil {
					t.Fatal("已确认提交的结果丢失", err)
				}
				return
			}
			if entry != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("输入或事务错误未传播", err)
			}
			if strings.HasPrefix(mode, "zero ") || mode == "nil context" || mode == "canceled input" {
				if calls != 0 {
					t.Fatal("无效输入进入事务")
				}
			}
		})
	}
	t.Run("missing dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺失事务方法未拒绝")
			}
		}()
		NewMoveEntryService(nil)
	})
}

func TestMoveEntryServiceConcurrentCalls(t *testing.T) {
	// 1. 多个调用共享服务，每个事务使用自己的读取和写入状态。
	mover := NewMoveEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, MoveEntryTransaction) error) error {
		_, transaction := moveEntryServiceFixture()
		return action(ctx, transaction)
	})
	var wait sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 20; iteration++ {
				actor, _ := moveEntryServiceFixture()
				entry, err := mover.Move(context.Background(), actor, 13, 3, 14)
				if err != nil || entry == nil || entry.Revision != 4 || *entry.ParentID != 14 {
					t.Error("并发结果错误", err)
					return
				}
			}
		}()
	}
	wait.Wait()
}
