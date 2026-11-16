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

type recordAccessTransactionStub struct {
	*fileEntryPathReaderStub
	result       *domain.EntryPreference
	writeError   error
	writeCalls   int
	writeContext context.Context
	entryID      uint64
	pathAtWrite  []uint64
	afterWrite   func()
}

func (transaction *recordAccessTransactionStub) RecordUserEntryAccess(ctx context.Context, entryID uint64) (*domain.EntryPreference, error) {
	transaction.writeCalls++
	transaction.writeContext, transaction.entryID = ctx, entryID
	transaction.pathAtWrite = append([]uint64(nil), transaction.entryIDs...)
	if transaction.afterWrite != nil {
		transaction.afterWrite()
	}
	return transaction.result, transaction.writeError
}

func recordAccessServiceFixture() (domain.Actor, *recordAccessTransactionStub) {
	actor, reader := fileEntryPathFixture()
	starredAt, openedAt := reader.entries[13].CreatedAt, reader.entries[13].CreatedAt.Add(time.Hour)
	return actor, &recordAccessTransactionStub{fileEntryPathReaderStub: reader, result: &domain.EntryPreference{UserID: actor.UserID, EntryID: 13, StarredAt: &starredAt, LastOpenedAt: &openedAt}}
}

func TestRecordAccessServiceResults(t *testing.T) {
	for _, mode := range []string{"preview", "open", "no star", "empty file", "maximum identifiers and revision", "date bounds", "offset dates"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := recordAccessServiceFixture()
			entryID, purpose := uint64(13), "preview"
			switch mode {
			case "open":
				purpose = "open"
			case "no star":
				transaction.result.StarredAt = nil
			case "empty file":
				transaction.entries[13].FileSize = 0
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
			case "date bounds":
				*transaction.result.StarredAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				*transaction.result.LastOpenedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "offset dates":
				*transaction.result.LastOpenedAt = transaction.result.LastOpenedAt.In(time.FixedZone("fixture", 3600))
			}
			transaction.result.UserID, transaction.result.EntryID = actor.UserID, entryID
			want := *transaction.result
			want.StarredAt, want.LastOpenedAt = copyOptionalValue(want.StarredAt), copyOptionalValue(want.LastOpenedAt)
			base := *transaction.entries[entryID]
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "access")
			calls := 0
			accessService := NewRecordAccessService(func(ctx context.Context, current domain.Actor, action func(context.Context, RecordAccessTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口上下文或身份错误")
				}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				// 提交确认前修改依赖返回值，核对服务已经取得独立副本。
				transaction.result.EntryID = 999
				*transaction.result.LastOpenedAt = time.Time{}
				if transaction.result.StarredAt != nil {
					*transaction.result.StarredAt = time.Time{}
				}
				return nil
			})
			result, err := accessService.Record(requestContext, actor, entryID, purpose)
			if err != nil || !reflect.DeepEqual(result, &want) || calls != 1 || transaction.writeCalls != 1 || transaction.writeContext != txContext || transaction.entryID != entryID || !reflect.DeepEqual(transaction.pathAtWrite, []uint64{entryID, 12, 11}) {
				t.Fatal("访问结果、顺序或事务参数错误", err)
			}
			if result == transaction.result || result.LastOpenedAt == transaction.result.LastOpenedAt || (result.StarredAt != nil && result.StarredAt == transaction.result.StarredAt) {
				t.Fatal("偏好副本错误")
			}
			for index, ctx := range transaction.contexts {
				if ctx != txContext || transaction.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未复用当前事务")
				}
			}
			if !reflect.DeepEqual(*transaction.entries[entryID], base) {
				t.Fatal("访问服务修改了基础记录")
			}
		})
	}
}

func TestRecordAccessServiceFailures(t *testing.T) {
	failure := errors.New("fixture access service failure")
	for _, testCase := range []struct {
		name   string
		change func(*recordAccessTransactionStub)
		want   error
		writes int
	}{
		{"missing target", func(tx *recordAccessTransactionStub) { delete(tx.entries, 13) }, domain.ErrFileEntryNotFound, 0},
		{"trashed target", func(tx *recordAccessTransactionStub) { tx.entries[13].Status = "TRASHED" }, domain.ErrFileEntryNotFound, 0},
		{"purging ancestor", func(tx *recordAccessTransactionStub) { tx.entries[12].Status = "PURGING" }, domain.ErrFileEntryNotFound, 0},
		{"purged ancestor", func(tx *recordAccessTransactionStub) { tx.entries[12].Status = "PURGED" }, domain.ErrFileEntryNotFound, 0},
		{"disabled", func(tx *recordAccessTransactionStub) { tx.profile.Status = "DISABLED" }, domain.ErrAccountDisabled, 0},
		{"foreign target", func(tx *recordAccessTransactionStub) { tx.entries[13].UserID++ }, domain.ErrInvalidFileEntryData, 0},
		{"missing ancestor", func(tx *recordAccessTransactionStub) { delete(tx.entries, 12) }, domain.ErrInvalidFileEntryData, 0},
		{"directory", func(tx *recordAccessTransactionStub) { tx.entries[13].EntryType = "DIR"; tx.entries[13].FileSize = 0 }, domain.ErrFileEntryNotFile, 0},
		{"root", func(tx *recordAccessTransactionStub) {}, domain.ErrFileEntryNotFile, 0},
		{"profile failure", func(tx *recordAccessTransactionStub) { tx.profileError = failure }, failure, 0},
		{"path failure", func(tx *recordAccessTransactionStub) { tx.entryErrors[12] = failure }, failure, 0},
		{"write failure", func(tx *recordAccessTransactionStub) { tx.writeError = failure }, failure, 1},
		{"locked hidden file", func(tx *recordAccessTransactionStub) { tx.writeError = domain.ErrFileEntryNotFound }, domain.ErrFileEntryNotFound, 1},
		{"nil result", func(tx *recordAccessTransactionStub) { tx.result = nil }, domain.ErrInvalidFileEntryData, 1},
		{"wrong owner", func(tx *recordAccessTransactionStub) { tx.result.UserID++ }, domain.ErrInvalidFileEntryData, 1},
		{"wrong target", func(tx *recordAccessTransactionStub) { tx.result.EntryID++ }, domain.ErrInvalidFileEntryData, 1},
		{"missing access", func(tx *recordAccessTransactionStub) { tx.result.LastOpenedAt = nil }, domain.ErrInvalidFileEntryData, 1},
		{"zero access", func(tx *recordAccessTransactionStub) { *tx.result.LastOpenedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"zero star", func(tx *recordAccessTransactionStub) { *tx.result.StarredAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"early access", func(tx *recordAccessTransactionStub) {
			*tx.result.LastOpenedAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
		}, domain.ErrInvalidFileEntryData, 1},
		{"late star", func(tx *recordAccessTransactionStub) {
			*tx.result.StarredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}, domain.ErrInvalidFileEntryData, 1},
		{"offset crosses minimum", func(tx *recordAccessTransactionStub) {
			*tx.result.LastOpenedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("fixture", 3600))
		}, domain.ErrInvalidFileEntryData, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, transaction := recordAccessServiceFixture()
			testCase.change(transaction)
			entryID := uint64(13)
			if testCase.name == "root" {
				entryID = 11
			}
			accessService := NewRecordAccessService(func(ctx context.Context, _ domain.Actor, action func(context.Context, RecordAccessTransaction) error) error {
				return action(ctx, transaction)
			})
			result, err := accessService.Record(context.Background(), actor, entryID, "preview")
			if result != nil || !errors.Is(err, testCase.want) || transaction.writeCalls != testCase.writes {
				t.Fatal("访问错误链、空结果或写入次数错误", err)
			}
		})
	}
}

func TestRecordAccessServicePurposes(t *testing.T) {
	for _, purpose := range []string{"", "PREVIEW", "Open", "download", " preview", "open ", "open\x00", "open\xff"} {
		t.Run(purpose, func(t *testing.T) {
			actor, _ := recordAccessServiceFixture()
			accessService := NewRecordAccessService(func(context.Context, domain.Actor, func(context.Context, RecordAccessTransaction) error) error {
				t.Error("非法用途进入事务")
				return nil
			})
			result, err := accessService.Record(context.Background(), actor, 13, purpose)
			if result != nil || !errors.Is(err, domain.ErrInvalidEntryAccessPurpose) {
				t.Fatal("非法用途未拒绝", err)
			}
		})
	}
}

func TestRecordAccessServiceInputsAndTransaction(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero id", "canceled input", "expired input", "nil tx context", "nil store", "canceled tx", "canceled profile", "canceled path", "canceled write", "expired tx", "outer failure", "canceled before commit", "commit failure", "no callback", "canceled after commit"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := recordAccessServiceFixture()
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
			accessService := NewRecordAccessService(func(ctx context.Context, _ domain.Actor, action func(context.Context, RecordAccessTransaction) error) error {
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
			result, err := accessService.Record(requestContext, actor, entryID, "preview")
			if mode == "canceled after commit" {
				if err != nil || result == nil || result.LastOpenedAt == nil {
					t.Fatal("已确认提交的访问结果丢失", err)
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
		NewRecordAccessService(nil)
	})
}
