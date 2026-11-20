package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type downloadAccessTransactionStub struct {
	*recordAccessTransactionStub
	content                       *domain.FileEntryContent
	contentError                  error
	contentCalls                  int
	contentContext                context.Context
	queriedUserID, queriedEntryID uint64
}

func (transaction *downloadAccessTransactionStub) FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error) {
	transaction.contentCalls++
	transaction.contentContext, transaction.queriedUserID, transaction.queriedEntryID = ctx, userID, entryID
	return transaction.content, transaction.contentError
}

func TestRecordDownloadAccess(t *testing.T) {
	failure := errors.New("fixture database failure")
	for _, scenario := range []struct {
		name                     string
		change                   func(*downloadAccessTransactionStub)
		contentCalls, writeCalls int
		wantError                error
	}{
		{name: "same file version", contentCalls: 1, writeCalls: 1},
		{name: "hidden ancestor", change: func(tx *downloadAccessTransactionStub) { tx.entries[12].Status = "TRASHED" }, wantError: domain.ErrFileEntryNotFound},
		{name: "now a directory", change: func(tx *downloadAccessTransactionStub) {
			tx.entries[13].EntryType, tx.entries[13].FileSize = "DIR", 0
		}, wantError: domain.ErrFileEntryNotFile},
		{name: "new revision", change: func(tx *downloadAccessTransactionStub) { tx.entries[13].Revision++ }, wantError: domain.ErrEntryRevisionConflict},
		{name: "changed object", change: func(tx *downloadAccessTransactionStub) { tx.content.ObjectID = new(uint64(42)) },
			contentCalls: 1, wantError: domain.ErrDownloadTargetUnavailable},
		{name: "no object", change: func(tx *downloadAccessTransactionStub) { tx.content.ObjectID = nil },
			contentCalls: 1, wantError: domain.ErrDownloadTargetUnavailable},
		{name: "wrong content owner", change: func(tx *downloadAccessTransactionStub) { tx.content.UserID++ },
			contentCalls: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "content read failed", change: func(tx *downloadAccessTransactionStub) { tx.contentError = failure },
			contentCalls: 1, wantError: failure},
		{name: "access write failed", change: func(tx *downloadAccessTransactionStub) { tx.writeError = failure },
			contentCalls: 1, writeCalls: 1, wantError: failure},
		{name: "missing recorded timestamp", change: func(tx *downloadAccessTransactionStub) { tx.result.LastOpenedAt = nil },
			contentCalls: 1, writeCalls: 1, wantError: domain.ErrInvalidFileEntryData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 提供签名后的目标和事务时刻的文件事实。
			actor, accessStore := recordAccessServiceFixture()
			target := &domain.EntryDownloadTarget{EntryID: 13, UserID: actor.UserID, Revision: 3, ObjectID: 41}
			transaction := &downloadAccessTransactionStub{recordAccessTransactionStub: accessStore,
				content: &domain.FileEntryContent{ID: target.EntryID, UserID: actor.UserID, ObjectID: new(target.ObjectID)}}
			if scenario.change != nil {
				scenario.change(transaction)
			}
			txContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			downloads := &EntryDownloadService{withAccessTransaction: func(ctx context.Context, identity domain.Actor, action func(context.Context, DownloadAccessTransaction) error) error {
				if ctx != t.Context() || identity != actor {
					t.Fatal("下载访问事务输入错误")
				}
				return action(txContext, transaction)
			}}

			// 2. 核对复查是否在写入前拒绝，并保留事务错误类别。
			err := downloads.recordDownloadAccess(t.Context(), actor, target)
			if !errors.Is(err, scenario.wantError) || transaction.contentCalls != scenario.contentCalls || transaction.writeCalls != scenario.writeCalls {
				t.Fatal("下载访问的错误或事务调用次数不符", err, transaction.contentCalls, transaction.writeCalls)
			}
			if transaction.contentCalls != 0 && (transaction.contentContext != txContext ||
				transaction.queriedUserID != actor.UserID || transaction.queriedEntryID != target.EntryID ||
				!reflect.DeepEqual(transaction.entryIDs, []uint64{13, 12, 11})) {
				t.Fatal("内容复查未按当前用户和已读取的路径执行")
			}
			if transaction.writeCalls != 0 && (transaction.writeContext != txContext || transaction.entryID != target.EntryID) {
				t.Fatal("最近访问未使用同一事务与目标")
			}
		})
	}
}
