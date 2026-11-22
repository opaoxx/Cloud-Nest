package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

type syncSnapshotCreationFixtureTransaction struct {
	userID     uint64
	now        time.Time
	generation domain.SyncCursorGeneration
	receipt    *domain.IdempotencyRecord
}

func (fixture *syncSnapshotCreationFixtureTransaction) FindUserIdempotencyRecord(context.Context, uint64, string, string) (*domain.IdempotencyRecord, error) {
	if fixture.receipt == nil {
		return nil, domain.ErrIdempotencyRecordNotFound
	}
	return fixture.receipt, nil
}

func (*syncSnapshotCreationFixtureTransaction) ValidateSyncSnapshotSequence(context.Context, uint64) error {
	return nil
}

func (fixture *syncSnapshotCreationFixtureTransaction) ResetSyncDeviceBaseline(_ context.Context, generation domain.SyncCursorGeneration) error {
	fixture.generation = generation
	return nil
}

func (fixture *syncSnapshotCreationFixtureTransaction) CreateUserIdempotencyRecord(_ context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	fixture.receipt = &domain.IdempotencyRecord{
		ID: 1, UserID: fixture.userID, CreatedAt: fixture.now,
		Operation: input.Operation, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash,
		ResourceType: input.ResourceType, ResponseStatus: input.ResponseStatus, ResponseBody: input.ResponseBody, ExpiresAt: input.ExpiresAt,
	}
	return fixture.receipt, nil
}

func TestSyncSnapshotCreationKeepsCacheAccordingToTransactionOutcome(t *testing.T) {
	transactionFailure := errors.New("fixture transaction failure")
	for _, testCase := range []struct {
		name      string
		wantError error
		keepCache bool
	}{
		{name: "committed", keepCache: true},
		{name: "concurrent receipt selected", keepCache: true},
		{name: "failed before commit", wantError: transactionFailure},
		{name: "commit result unconfirmed", wantError: domain.ErrUserTransactionCommitUnconfirmed, keepCache: true},
		{name: "request canceled after commit", wantError: context.Canceled, keepCache: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 用真实缓存和签发器组装服务，分别控制事务提交与请求取消。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stamp := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
			clock := func() time.Time { return stamp }
			cache := NewSyncSnapshotCache(clock)
			binding, snapshot := syncSnapshotCacheFixture()
			actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 71}
			const key = "snapshot-outcome"
			committed := syncSnapshotCreationFixtureTransaction{userID: actor.UserID, now: stamp}
			winner := committed
			if testCase.name == "concurrent receipt selected" {
				putSyncSnapshotForReadTest(t, cache, binding, snapshot)
				body, err := dto.EncodeCreateSyncSnapshotReceipt(domain.SyncSnapshotReceiptLocator{
					SnapshotID: binding.SnapshotID, DeviceID: binding.DeviceID, Generation: binding.Generation,
				})
				if err != nil {
					t.Fatal(err)
				}
				hash, err := BuildCreateSyncSnapshotRequestHash(actor.DeviceID, binding.PageSize)
				if err != nil {
					t.Fatal(err)
				}
				winner.generation = binding.Generation
				if _, err := winner.CreateUserIdempotencyRecord(ctx, domain.IdempotencyResultInput{
					Operation: domain.CreateSyncSnapshotOperation, IdempotencyKey: key, RequestHash: hash,
					ResourceType: domain.SyncSnapshotResourceType, ResponseStatus: 201, ResponseBody: body,
					ExpiresAt: stamp.Add(syncSnapshotCreationReceiptRetention),
				}); err != nil {
					t.Fatal(err)
				}
			}
			builds, transactions, generationReads := 0, 0, 0
			preparation := NewSyncSnapshotPreparation(func(context.Context, domain.Actor) (*BuiltSyncSnapshot, error) {
				builds++
				return snapshot, nil
			}, cache)
			creator := NewSyncSnapshotCreationService(
				preparation, func(requestContext context.Context, _ domain.Actor, action func(context.Context, SyncSnapshotFinalizationTransaction) error) error {
					transactions++
					if transactions == 2 && testCase.name == "concurrent receipt selected" {
						committed = winner
					}
					pending := committed
					if err := action(requestContext, &pending); err != nil {
						return err
					}
					if transactions == 2 && testCase.name == "failed before commit" {
						return transactionFailure
					}
					committed = pending
					if transactions == 2 {
						switch testCase.name {
						case "commit result unconfirmed":
							return errors.Join(domain.ErrUserTransactionCommitUnconfirmed, transactionFailure)
						case "request canceled after commit":
							cancel()
						}
					}
					return nil
				}, func(context.Context, uint64, uint64) (domain.SyncCursorGeneration, error) {
					generationReads++
					return committed.generation, nil
				}, syncSnapshotReplayDeliveryFixture(t), dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt, clock,
			)

			// 2. 只有确认提交且请求仍有效时才返回首页。
			page, err := creator.Create(ctx, actor, key, binding.PageSize)
			if !errors.Is(err, testCase.wantError) || (page != nil) != (testCase.wantError == nil) || transactions != 2 || builds != 1 {
				t.Fatalf("首页=%+v，错误=%v，事务=%d，构建=%d", page, err, transactions, builds)
			}
			if testCase.wantError != nil && generationReads != 0 {
				t.Fatal("错误后仍开始读首页")
			}
			if !testCase.keepCache {
				assertSnapshotCacheCapacity(t, cache, 0, 0, 0)
				return
			}

			// 3. 提交、提交结果不明或提交后取消的候选可供同键重试。
			assertSnapshotCacheCapacity(t, cache, 0, 1, snapshot.Entries.SizeBytes)
			retried, err := creator.Create(t.Context(), actor, key, binding.PageSize)
			locator, decodeError := dto.DecodeCreateSyncSnapshotReceipt(committed.receipt.ResponseBody)
			if err != nil || decodeError != nil || retried == nil || locator == nil || retried.SnapshotID != locator.SnapshotID ||
				retried.Header != snapshot.Header || !retried.ExpiresAt.Equal(stamp.Add(syncSnapshotCacheTTL)) ||
				retried.BaselineToken == nil || builds != 1 || transactions != 3 {
				t.Fatalf("重试首页=%+v，定位=%+v，错误=%v，解码错误=%v，构建=%d，事务=%d", retried, locator, err, decodeError, builds, transactions)
			}
		})
	}
}
