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
	// 1. 装配只读快照构建、共享缓存和分页签发器。
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
	delivery := service.NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign)
	cache := service.NewSyncSnapshotCache(time.Now)
	builder := service.NewSyncSnapshotBuilder(
		func(ctx context.Context, userID uint64, read func(context.Context, service.SyncSnapshotReader, domain.SyncSnapshotHeader) error) error {
			return store.WithUserSyncSnapshot(ctx, userID, func(readContext context.Context, snapshotStore *repository.Store, header domain.SyncSnapshotHeader) error {
				return read(readContext, snapshotStore, header)
			})
		}, dto.EncodeSyncSnapshotEntries)

	// 2. 绑定创建事务与续页读取，共用本次启动的缓存和交付器。
	creator := service.NewSyncSnapshotCreationService(service.NewSyncSnapshotPreparation(builder.Build, cache),
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SyncSnapshotFinalizationTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		}, store.FindSyncDeviceGeneration, delivery, dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt, time.Now)
	continuation := service.NewSyncSnapshotContinuationService(cursorVerifier.Verify, store.FindSyncDeviceGeneration, cache.Get, time.Now)

	// 3. 把创建和续页挂到实际启动的 HTTP 路由器，共用 Bearer 授权。
	serverhttp.RegisterCreateSyncSnapshotRoute(router, authorize, creator.Create)
	serverhttp.RegisterSyncSnapshotPageRoute(router, authorize,
		func(ctx context.Context, actor domain.Actor, snapshotID uuid.UUID, cursor string) (*domain.SyncSnapshotPageResult, error) {
			page, err := continuation.Read(ctx, actor, snapshotID, cursor)
			if err != nil {
				return nil, err
			}
			return delivery.Build(ctx, page.Binding, page.Header, page.ExpiresAt, &page.Page)
		})
	return nil
}
