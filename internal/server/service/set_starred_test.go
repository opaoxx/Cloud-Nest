package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type setStarredTransactionStub struct {
	*fileEntryPathReaderStub
	result       *domain.EntryPreference
	writeError   error
	writeCalls   int
	writeContext context.Context
	entryID      uint64
	starred      bool
	afterWrite   func()
}

func (transaction *setStarredTransactionStub) SetUserEntryStarred(ctx context.Context, entryID uint64, starred bool) (*domain.EntryPreference, error) {
	transaction.writeCalls++
	transaction.writeContext, transaction.entryID, transaction.starred = ctx, entryID, starred
	if transaction.afterWrite != nil {
		transaction.afterWrite()
	}
	return transaction.result, transaction.writeError
}

func setStarredServiceFixture() (domain.Actor, *setStarredTransactionStub) {
	actor, reader := fileEntryPathFixture()
	starredAt, openedAt := reader.entries[13].CreatedAt, reader.entries[13].CreatedAt.Add(time.Hour)
	return actor, &setStarredTransactionStub{
		fileEntryPathReaderStub: reader,
		result:                  &domain.EntryPreference{UserID: actor.UserID, EntryID: 13, StarredAt: &starredAt, LastOpenedAt: &openedAt},
	}
}

func TestSetStarredServiceResults(t *testing.T) {
	for _, mode := range []string{"file", "directory", "root", "unstar", "missing preference", "maximum identifiers and revision", "minimum and maximum dates", "offset dates"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := setStarredServiceFixture()
			entryID, starred := uint64(13), true
			switch mode {
			case "directory":
				entryID = 12
			case "root":
				entryID = 11
			case "unstar", "missing preference":
				starred, transaction.result.StarredAt = false, nil
				if mode == "missing preference" {
					transaction.result.LastOpenedAt = nil
				}
			case "maximum identifiers and revision":
				entryID = ^uint64(0)
				actor = domain.Actor{UserID: entryID, DeviceID: entryID, SessionID: entryID}
				transaction.profile.ID = actor.UserID
				for _, entry := range transaction.entries {
					entry.UserID = actor.UserID
				}
				file := transaction.entries[13]
				delete(transaction.entries, 13)
				file.ID, file.Revision = entryID, entryID
				transaction.entries[entryID] = file
			case "minimum and maximum dates":
				*transaction.result.StarredAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				*transaction.result.LastOpenedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "offset dates":
				*transaction.result.StarredAt = transaction.result.StarredAt.In(time.FixedZone("fixture", 8*60*60))
			}
			transaction.result.UserID, transaction.result.EntryID = actor.UserID, entryID
			want := *transaction.result
			want.StarredAt, want.LastOpenedAt = copyOptionalValue(want.StarredAt), copyOptionalValue(want.LastOpenedAt)
			base := *transaction.entries[entryID]
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "starred")
			calls := 0
			starredService := NewSetStarredService(func(ctx context.Context, current domain.Actor, action func(context.Context, SetStarredTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口的上下文或身份错误")
				}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				// 回调已结束、提交确认前修改依赖结果，服务应已持有副本。
				transaction.result.EntryID = 999
				if transaction.result.StarredAt != nil {
					*transaction.result.StarredAt = time.Time{}
				}
				if transaction.result.LastOpenedAt != nil {
					*transaction.result.LastOpenedAt = time.Time{}
				}
				return nil
			})
			result, err := starredService.Set(requestContext, actor, entryID, starred)
			if err != nil || !reflect.DeepEqual(result, &want) || calls != 1 || transaction.writeCalls != 1 || transaction.entryID != entryID || transaction.starred != starred || transaction.writeContext != txContext {
				t.Fatal("收藏结果、状态参数或单次事务错误", err)
			}
			if result == transaction.result || (result.StarredAt != nil && result.StarredAt == transaction.result.StarredAt) || (result.LastOpenedAt != nil && result.LastOpenedAt == transaction.result.LastOpenedAt) {
				t.Fatal("偏好结果未独立复制")
			}
			for index, ctx := range transaction.contexts {
				if ctx != txContext || transaction.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未复用事务上下文及用户")
				}
			}
			wantPath := []uint64{entryID, 12, 11}
			if entryID == 12 {
				wantPath = []uint64{12, 11}
			}
			if entryID == 11 {
				wantPath = []uint64{11}
			}
			if !reflect.DeepEqual(transaction.entryIDs, wantPath) || !reflect.DeepEqual(*transaction.entries[entryID], base) {
				t.Fatal("完整路径或目录项基础事实错误")
			}
		})
	}
}

func TestSetStarredServiceFailures(t *testing.T) {
	failure := errors.New("fixture starred failure")
	for _, testCase := range []struct {
		name   string
		change func(*setStarredTransactionStub)
		want   error
		writes int
	}{
		{"missing target", func(tx *setStarredTransactionStub) { delete(tx.entries, 13) }, domain.ErrFileEntryNotFound, 0},
		{"trashed target", func(tx *setStarredTransactionStub) { tx.entries[13].Status = "TRASHED" }, domain.ErrFileEntryNotFound, 0},
		{"purging ancestor", func(tx *setStarredTransactionStub) { tx.entries[12].Status = "PURGING" }, domain.ErrFileEntryNotFound, 0},
		{"purged ancestor", func(tx *setStarredTransactionStub) { tx.entries[12].Status = "PURGED" }, domain.ErrFileEntryNotFound, 0},
		{"disabled", func(tx *setStarredTransactionStub) { tx.profile.Status = "DISABLED" }, domain.ErrAccountDisabled, 0},
		{"foreign target", func(tx *setStarredTransactionStub) { tx.entries[13].UserID++ }, domain.ErrInvalidFileEntryData, 0},
		{"missing ancestor", func(tx *setStarredTransactionStub) { delete(tx.entries, 12) }, domain.ErrInvalidFileEntryData, 0},
		{"cycle", func(tx *setStarredTransactionStub) { tx.entries[12].ParentID = copyOptionalValue(&tx.entries[13].ID) }, domain.ErrInvalidFileEntryData, 0},
		{"profile failure", func(tx *setStarredTransactionStub) { tx.profileError = failure }, failure, 0},
		{"path failure", func(tx *setStarredTransactionStub) { tx.entryErrors[12] = failure }, failure, 0},
		{"write failure", func(tx *setStarredTransactionStub) { tx.writeError = failure }, failure, 1},
		{"locked hidden target", func(tx *setStarredTransactionStub) { tx.writeError = domain.ErrFileEntryNotFound }, domain.ErrFileEntryNotFound, 1},
		{"nil preference", func(tx *setStarredTransactionStub) { tx.result = nil }, domain.ErrInvalidFileEntryData, 1},
		{"wrong owner", func(tx *setStarredTransactionStub) { tx.result.UserID++ }, domain.ErrInvalidFileEntryData, 1},
		{"wrong entry", func(tx *setStarredTransactionStub) { tx.result.EntryID++ }, domain.ErrInvalidFileEntryData, 1},
		{"missing star", func(tx *setStarredTransactionStub) { tx.result.StarredAt = nil }, domain.ErrInvalidFileEntryData, 1},
		{"remaining star on false", func(tx *setStarredTransactionStub) {}, domain.ErrInvalidFileEntryData, 1},
		{"zero star", func(tx *setStarredTransactionStub) { *tx.result.StarredAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"zero opened", func(tx *setStarredTransactionStub) { *tx.result.LastOpenedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"early star", func(tx *setStarredTransactionStub) {
			*tx.result.StarredAt = time.Date(999, 12, 31, 0, 0, 0, 0, time.UTC)
		}, domain.ErrInvalidFileEntryData, 1},
		{"late opened", func(tx *setStarredTransactionStub) {
			*tx.result.LastOpenedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}, domain.ErrInvalidFileEntryData, 1},
		{"offset crosses minimum", func(tx *setStarredTransactionStub) {
			*tx.result.StarredAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("fixture", 3600))
		}, domain.ErrInvalidFileEntryData, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, transaction := setStarredServiceFixture()
			testCase.change(transaction)
			starredService := NewSetStarredService(func(ctx context.Context, _ domain.Actor, action func(context.Context, SetStarredTransaction) error) error {
				return action(ctx, transaction)
			})
			result, err := starredService.Set(context.Background(), actor, 13, testCase.name != "remaining star on false")
			if result != nil || !errors.Is(err, testCase.want) || transaction.writeCalls != testCase.writes {
				t.Fatal("错误链、空结果或写入次数错误", err)
			}
		})
	}
}

func TestSetStarredServiceInputsAndTransaction(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero id", "canceled input", "expired input", "nil tx context", "nil store", "canceled tx", "canceled profile", "canceled path", "canceled write", "expired tx", "outer failure", "canceled before commit", "commit failure", "no callback", "canceled after commit"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := setStarredServiceFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requestContext context.Context = ctx
			entryID := uint64(13)
			failure := errors.New("fixture commit failed")
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
			case "zero id":
				entryID = 0
			case "canceled input":
				cancel()
				want = context.Canceled
			case "expired input":
				var cancelDeadline context.CancelFunc
				requestContext, cancelDeadline = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancelDeadline()
				want = context.DeadlineExceeded
			case "canceled profile":
				transaction.afterProfile, want = cancel, context.Canceled
			case "canceled path":
				transaction.afterEntry, want = func(uint64) { cancel() }, context.Canceled
			case "canceled write":
				transaction.afterWrite, transaction.writeError, want = cancel, failure, context.Canceled
			case "canceled tx", "canceled before commit":
				want = context.Canceled
			case "expired tx":
				want = context.DeadlineExceeded
			case "outer failure", "commit failure":
				want = failure
			case "no callback":
				want = domain.ErrInvalidFileEntryData
			}
			calls := 0
			starredService := NewSetStarredService(func(ctx context.Context, _ domain.Actor, action func(context.Context, SetStarredTransaction) error) error {
				calls++
				switch mode {
				case "nil tx context":
					return action(nil, transaction)
				case "nil store":
					return action(ctx, nil)
				case "canceled tx":
					cancel()
				case "expired tx":
					expired, cancelDeadline := context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancelDeadline()
					return action(expired, transaction)
				case "no callback":
					return nil
				case "outer failure":
					return failure
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				switch mode {
				case "canceled before commit":
					cancel()
					return ctx.Err()
				case "commit failure":
					return failure
				case "canceled after commit":
					cancel()
				}
				return nil
			})
			result, err := starredService.Set(requestContext, actor, entryID, true)
			if mode == "canceled after commit" {
				if err != nil || result == nil || result.StarredAt == nil {
					t.Fatal("已确认提交的收藏结果丢失", err)
				}
				return
			}
			if result != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("输入或事务错误未传播", err)
			}
			if strings.HasPrefix(mode, "zero ") || mode == "nil context" || mode == "canceled input" || mode == "expired input" {
				if calls != 0 {
					t.Fatal("非法输入进入事务")
				}
			}
			if mode != "canceled write" && mode != "canceled before commit" && mode != "commit failure" && transaction.writeCalls != 0 {
				t.Fatal("前置失败后发生了写入")
			}
		})
	}
	t.Run("missing dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺失事务方法未拒绝")
			}
		}()
		NewSetStarredService(nil)
	})
}
