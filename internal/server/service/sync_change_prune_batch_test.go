package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncChangePruneBatchPreservesProgress(t *testing.T) {
	cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
	uncertain := errors.Join(domain.ErrUserTransactionCommitUnconfirmed, context.Canceled)
	nextPageFailure := errors.New("下页查询失败")
	oneDeleted := domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 18, Count: 1}
	for _, fixture := range []struct {
		name         string
		failNextPage bool
		secondError  error
		wantError    error
		wantScanned  int
		wantDeleted  int
		wantAfter    uint64
		wantFailures int
	}{
		{name: "cancel before confirming second account", secondError: context.Canceled, wantError: context.Canceled, wantScanned: 4, wantDeleted: 1, wantAfter: 12},
		{name: "second account commits before cancel", wantError: context.Canceled, wantScanned: 4, wantDeleted: 2, wantAfter: 14},
		{name: "commit unconfirmed is counted as failure", secondError: uncertain, wantError: context.Canceled, wantScanned: 4, wantDeleted: 1, wantAfter: 14, wantFailures: 1},
		{name: "later scan fails", failNextPage: true, wantError: nextPageFailure, wantScanned: 2, wantDeleted: 1, wantAfter: 13},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads, calls := 0, 0
			batch := NewSyncChangePruneBatch(func(context.Context) (time.Time, error) {
				reads++
				return cutoff, nil
			}, func(_ context.Context, gotCutoff time.Time, after uint64, pageSize int) (domain.SyncChangePruneCandidatePage, error) {
				if gotCutoff != cutoff {
					t.Fatal("同一轮的分页截止点变动")
				}
				if fixture.failNextPage {
					if after == 13 {
						return domain.SyncChangePruneCandidatePage{}, nextPageFailure
					}
					if after != 11 || pageSize != 2 {
						t.Fatal("后续页失败场景的扫描参数不符")
					}
					return domain.SyncChangePruneCandidatePage{UserIDs: []uint64{12}, NextAfterUserID: 13, ScannedCount: 2}, nil
				}
				if after != 11 || pageSize != 4 {
					t.Fatal("取消场景的扫描参数不符")
				}
				return domain.SyncChangePruneCandidatePage{UserIDs: []uint64{12, 14, 15}, NextAfterUserID: 16, ScannedCount: 4}, nil
			}, func(_ context.Context, userID uint64, gotCutoff time.Time, limit int) (domain.SyncChangePruneRange, error) {
				calls++
				if gotCutoff != cutoff || limit != 1 || (calls == 1 && userID != 12) || (calls == 2 && userID != 14) || calls > 2 {
					t.Fatal("发生了不符合预算、顺序或截止点的裁剪调用")
				}
				if calls == 2 {
					cancel()
					return oneDeleted, fixture.secondError
				}
				return oneDeleted, nil
			})
			pageSize := 4
			if fixture.failNextPage {
				pageSize = 2
			}
			got, err := batch.RunOnce(ctx, 11, SyncChangePruneBatchLimits{ScanLimit: 4, PageSize: pageSize, AccountDeleteLimit: 1})
			if !errors.Is(err, fixture.wantError) || reads != 1 || got.Scanned != fixture.wantScanned ||
				got.PrunedAccounts != fixture.wantDeleted || got.DeletedEvents != fixture.wantDeleted ||
				got.NextAfterUserID != fixture.wantAfter || len(got.AccountErrors) != fixture.wantFailures {
				t.Fatalf("中断结果不符：%+v，错误 %v，读截止点 %d 次", got, err, reads)
			}
			if fixture.wantFailures > 0 && (got.AccountErrors[0].UserID != 14 || !errors.Is(got.AccountErrors[0].Err, domain.ErrUserTransactionCommitUnconfirmed)) {
				t.Fatalf("提交未确认的账号结果丢失：%+v", got.AccountErrors)
			}
		})
	}
}

func TestSyncChangePruneBatchRejectsInvalidInputs(t *testing.T) {
	cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
	readFailed := errors.New("时钟查询失败")
	for _, fixture := range []struct {
		name  string
		stamp time.Time
		err   error
		page  domain.SyncChangePruneCandidatePage
	}{
		{name: "clock error", err: readFailed},
		{name: "submicrosecond clock", stamp: cutoff.Add(time.Nanosecond)},
		{name: "oversized page", stamp: cutoff, page: domain.SyncChangePruneCandidatePage{NextAfterUserID: 16, ScannedCount: 3}},
		{name: "out of range candidate", stamp: cutoff, page: domain.SyncChangePruneCandidatePage{UserIDs: []uint64{12, 15}, NextAfterUserID: 14, ScannedCount: 2}},
		{name: "cursor without scan progress", stamp: cutoff, page: domain.SyncChangePruneCandidatePage{NextAfterUserID: 12}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			readCalls := 0
			batch := NewSyncChangePruneBatch(func(context.Context) (time.Time, error) {
				readCalls++
				return fixture.stamp, fixture.err
			}, func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error) {
				if fixture.stamp != cutoff {
					t.Fatal("不可用的截止点进入扫描")
				}
				return fixture.page, nil
			}, func(context.Context, uint64, time.Time, int) (domain.SyncChangePruneRange, error) {
				t.Fatal("无效输入进入账号裁剪")
				return domain.SyncChangePruneRange{}, nil
			})
			for _, limits := range []SyncChangePruneBatchLimits{
				{ScanLimit: 0, PageSize: 2, AccountDeleteLimit: 1},
				{ScanLimit: MaxSyncChangePruneAccountsPerRound + 1, PageSize: 2, AccountDeleteLimit: 1},
				{ScanLimit: 2, PageSize: domain.MaxSyncChangePruneAccountPageSize + 1, AccountDeleteLimit: 1},
				{ScanLimit: 2, PageSize: 2, AccountDeleteLimit: domain.MaxSyncChangePruneBatchSize + 1},
			} {
				if got, err := batch.RunOnce(t.Context(), 11, limits); err == nil || readCalls != 0 || got.NextAfterUserID != 11 {
					t.Fatalf("非法限制没有被提前拒绝：%+v，%+v，%v", limits, got, err)
				}
			}

			got, err := batch.RunOnce(t.Context(), 11, SyncChangePruneBatchLimits{ScanLimit: 2, PageSize: 2, AccountDeleteLimit: 1})
			if err == nil || (fixture.err != nil && !errors.Is(err, fixture.err)) ||
				readCalls != 1 || got.Scanned != 0 || got.NextAfterUserID != 11 || len(got.AccountErrors) != 0 {
				t.Fatalf("错误或无效结果未按预期拒绝：%+v，%v，读钟 %d 次", got, err, readCalls)
			}
		})
	}
}
