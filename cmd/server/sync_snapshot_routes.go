package main

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func registerServerSyncSnapshotRoutes(
	router *gin.Engine, store *repository.Store, masterKey []byte,
	authorize func(context.Context, string) (*domain.Actor, error),
) error {
	// 1. 装配快照构建、共享缓存、分页签发与末页验签。
	cursorSigner, err := service.NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		return err
	}
	cursorVerifier, err := service.NewSyncSnapshotCursorVerifier(masterKey)
	if err != nil {
		return err
	}
	baselineSigner, err := service.NewSyncSnapshotBaselineSigner(masterKey)
	if err != nil {
		return err
	}
	baselineVerifier, err := service.NewSyncSnapshotBaselineVerifier(masterKey)
	if err != nil {
		return err
	}
	delivery := service.NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign)
	cache := service.NewSyncSnapshotCache(time.Now)
	builder := service.NewSyncSnapshotBuilder(
		func(ctx context.Context, userID uint64, read func(context.Context, service.SyncSnapshotReader, domain.SyncSnapshotHeader) error) error {
			return store.WithUserSyncSnapshot(ctx, userID, func(readContext context.Context, snapshotStore *repository.Store, header domain.SyncSnapshotHeader) error {
				return read(readContext, snapshotStore, header)
			})
		}, dto.EncodeSyncSnapshotEntries)

	// 2. 绑定创建、续页及确认服务。确认在已认证账号事务内复核和更新。
	creator := service.NewSyncSnapshotCreationService(service.NewSyncSnapshotPreparation(builder.Build, cache),
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SyncSnapshotFinalizationTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		}, store.FindSyncDeviceGeneration, delivery, dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt, time.Now)
	continuation := service.NewSyncSnapshotContinuationService(cursorVerifier.Verify, store.FindSyncDeviceGeneration, cache.Get, time.Now)
	confirmer := service.NewSyncSnapshotBaselineConfirmationService(baselineVerifier,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SyncSnapshotBaselineConfirmationTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		}, time.Now)

	// 3. 从同一主密钥装配增量游标签发与校验，将拉取绑定到只读一致性事务。
	changeCursorSigner, err := service.NewSyncChangeCursorSigner(masterKey)
	if err != nil {
		return err
	}
	changeCursorVerifier, err := service.NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		return err
	}
	pullResolver := service.NewSyncChangePullStartResolver(baselineVerifier, changeCursorVerifier, time.Now)
	puller := service.NewSyncChangePullService(pullResolver,
		func(ctx context.Context, actor domain.Actor, read func(context.Context, service.SyncChangePageReader, domain.SyncDeviceState) error) error {
			return store.WithSyncChangePullSnapshot(ctx, actor, func(readContext context.Context, snapshotStore *repository.Store, state domain.SyncDeviceState) error {
				return read(readContext, snapshotStore, state)
			})
		}, changeCursorSigner)

	// 4. 复用增量游标校验器，把确认服务绑定到账号事务。
	changeConfirmer := service.NewSyncChangeConfirmationService(changeCursorVerifier,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SyncChangeConfirmationTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})

	// 5. 在实际启动的路由器注册快照、增量拉取和增量确认，共用 Bearer 授权。
	serverhttp.RegisterCreateSyncSnapshotRoute(router, authorize, creator.Create)
	serverhttp.RegisterSyncSnapshotPageRoute(router, authorize,
		func(ctx context.Context, actor domain.Actor, snapshotID uuid.UUID, cursor string) (*domain.SyncSnapshotPageResult, error) {
			page, err := continuation.Read(ctx, actor, snapshotID, cursor)
			if err != nil {
				return nil, err
			}
			return delivery.Build(ctx, page.Binding, page.Header, page.ExpiresAt, &page.Page)
		})
	serverhttp.RegisterConfirmSyncSnapshotBaselineRoute(router, authorize, confirmer.Confirm)
	serverhttp.RegisterPullSyncChangesRoute(router, authorize, puller.Pull)
	serverhttp.RegisterConfirmSyncChangesRoute(router, authorize, changeConfirmer.Confirm)
	return nil
}
