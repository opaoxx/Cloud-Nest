package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type snapshotReceiptReaderFunc func(context.Context, uint64, string, string) (*domain.IdempotencyRecord, error)

func (find snapshotReceiptReaderFunc) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	return find(ctx, userID, operation, key)
}

func TestSyncSnapshotCreationReplayReadsAfterTransaction(t *testing.T) {
	key := bytes.Repeat([]byte{0x63}, sha256.Size)
	cursorSigner, err := service.NewSyncSnapshotCursorSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	baselineSigner, err := service.NewSyncSnapshotBaselineSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	delivery := service.NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign)
	transactionFailure := errors.New("fixture transaction failed")
	for _, testCase := range []struct {
		name               string
		want               error
		wantReadGeneration int
		wantReadCache      int
		wantPage           bool
	}{
		{name: "same request", wantReadGeneration: 1, wantReadCache: 1, wantPage: true},
		{name: "no receipt"},
		{name: "different parameters", want: domain.ErrIdempotencyRequestConflict},
		{name: "receipt expires while acquiring lock", want: domain.ErrIdempotencyRecordExpired},
		{name: "transaction fails after lookup", want: transactionFailure},
		{name: "cancel after lookup", want: context.Canceled},
		{name: "current generation differs", want: domain.ErrSyncSnapshotExpired, wantReadGeneration: 1},
		{name: "missing cache", want: domain.ErrSyncSnapshotExpired, wantReadGeneration: 1, wantReadCache: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备已提交回执和真实缓存；每个场景只改变一个条件。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			record, actor, locator := syncSnapshotReplayFixture(t)
			now := record.CreatedAt
			cache := service.NewSyncSnapshotCache(func() time.Time { return now })
			binding := service.SyncSnapshotCacheBinding{
				SnapshotID: locator.SnapshotID, UserID: actor.UserID, DeviceID: actor.DeviceID,
				Generation: locator.Generation, PageSize: syncSnapshotReplayPageSize,
			}
			var expiresAt time.Time
			header := domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 42}
			if testCase.name != "missing cache" {
				reservation, err := cache.ReserveBuild(ctx)
				if err != nil {
					t.Fatal(err)
				}
				expiresAt, err = cache.Put(ctx, reservation, binding, &service.BuiltSyncSnapshot{
					Header: header, Entries: domain.SyncSnapshotEntriesJSON{Items: []json.RawMessage{}, SizeBytes: 2},
				})
				if err != nil {
					t.Fatal(err)
				}
			}

			// 2. 模拟账号锁与事务结束，要求代次与缓存只用原请求上下文。
			inTransaction := false
			lookupCalls, generationCalls, cacheCalls := 0, 0, 0
			pageReader := service.NewSyncSnapshotReplayService(
				func(readContext context.Context, userID, deviceID uint64) (domain.SyncCursorGeneration, error) {
					generationCalls++
					if inTransaction || readContext != ctx || userID != actor.UserID || deviceID != actor.DeviceID {
						t.Fatal("读取已提交代次未使用正确的事务外上下文和归属")
					}
					generation := locator.Generation
					if testCase.name == "current generation differs" {
						generation[15] ^= 1
					}
					return generation, nil
				},
				func(readContext context.Context, cacheBinding service.SyncSnapshotCacheBinding) (*service.CachedSyncSnapshot, error) {
					cacheCalls++
					if inTransaction || readContext != ctx || cacheBinding != binding {
						t.Fatal("未在事务外使用原快照绑定")
					}
					return cache.Get(readContext, cacheBinding)
				}, delivery)
			replay := service.NewSyncSnapshotCreationReplay(
				func(requestContext context.Context, identity domain.Actor, action func(context.Context, service.SyncSnapshotReceiptReader) error) error {
					if requestContext != ctx || identity != actor {
						t.Fatal("开启账号事务时身份或上下文错误")
					}
					txContext, stop := context.WithCancel(requestContext)
					defer stop()
					inTransaction = true
					defer func() { inTransaction = false }()
					if testCase.name == "receipt expires while acquiring lock" {
						now = record.ExpiresAt
					}
					err := action(txContext, snapshotReceiptReaderFunc(func(queryContext context.Context, userID uint64, operation, queryKey string) (*domain.IdempotencyRecord, error) {
						lookupCalls++
						if !inTransaction || queryContext != txContext || userID != actor.UserID ||
							operation != domain.CreateSyncSnapshotOperation || queryKey != syncSnapshotReplayKey {
							t.Fatal("回执查询未使用当前账号事务和请求范围")
						}
						if testCase.name == "no receipt" {
							return nil, domain.ErrIdempotencyRecordNotFound
						}
						return &record, nil
					}))
					if testCase.name == "transaction fails after lookup" {
						return transactionFailure
					}
					if testCase.name == "cancel after lookup" {
						cancel()
					}
					return err
				}, dto.DecodeCreateSyncSnapshotReceipt, pageReader, func() time.Time { return now })

			// 3. 核对命中结果与失败分支，缺失和错误都不读取缓存。
			pageSize := syncSnapshotReplayPageSize
			if testCase.name == "different parameters" {
				pageSize++
			}
			page, err := replay.ReadExistingFirstPage(ctx, actor, syncSnapshotReplayKey, pageSize)
			if !errors.Is(err, testCase.want) || (page != nil) != testCase.wantPage ||
				lookupCalls != 1 || generationCalls != testCase.wantReadGeneration || cacheCalls != testCase.wantReadCache {
				t.Fatalf("首页=%+v，错误=%v，查回执=%d，查代次=%d，读缓存=%d", page, err, lookupCalls, generationCalls, cacheCalls)
			}
			if page != nil {
				token, err := baselineSigner.Sign(binding, header.HighWaterSequence, expiresAt)
				if err != nil || page.SnapshotID != binding.SnapshotID || page.Header != header || page.ExpiresAt != expiresAt ||
					page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil || page.BaselineToken == nil ||
					*page.BaselineToken != token {
					t.Fatalf("重读未交付原首页数据及凭据：%+v，%v", page, err)
				}
			}
		})
	}
}
