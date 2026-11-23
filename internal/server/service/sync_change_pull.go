package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangePullService 负责验签、读取并交付一页增量变更。
type SyncChangePullService struct {
	resolver     *SyncChangePullStartResolver
	withSnapshot func(context.Context, domain.Actor, func(context.Context, SyncChangePageReader, domain.SyncDeviceState) error) error
	signer       *SyncChangeCursorSigner
}

// NewSyncChangePullService 绑定凭据解析器、只读事务入口和续读游标签发器。
func NewSyncChangePullService(
	resolver *SyncChangePullStartResolver,
	withSnapshot func(context.Context, domain.Actor, func(context.Context, SyncChangePageReader, domain.SyncDeviceState) error) error,
	signer *SyncChangeCursorSigner,
) *SyncChangePullService {
	if resolver == nil || withSnapshot == nil || signer == nil {
		panic("增量拉取需要凭据解析器、只读事务和游标签发器")
	}
	return &SyncChangePullService{resolver: resolver, withSnapshot: withSnapshot, signer: signer}
}

// Pull 按已认证身份使用末页凭据或续读游标拉取一页变更。
func (puller *SyncChangePullService) Pull(
	ctx context.Context, actor domain.Actor, baselineToken, cursor string, pageSize int,
) (*domain.SyncChangePullResult, error) {
	// 1. 校验请求参数并验签，取得排他读取起点。
	if ctx == nil {
		return nil, fmt.Errorf("增量拉取需要请求上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("%w: 页大小 %d 无效", domain.ErrInvalidSyncChangePage, pageSize)
	}
	start, verifyError := puller.resolver.Resolve(actor, baselineToken, cursor)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifyError != nil {
		return nil, verifyError
	}

	// 2. 在同一个只读事务中读取并校验页面。
	var page *SyncChangeReadPage
	snapshotError := puller.withSnapshot(ctx, actor, func(readContext context.Context, reader SyncChangePageReader, state domain.SyncDeviceState) error {
		var readError error
		page, readError = ReadSyncChangePageInSnapshot(readContext, actor, start, state, pageSize, puller.resolver.now, reader)
		return readError
	})

	// 3. 等事务成功结束，再用外层请求上下文签发结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(snapshotError, domain.ErrSyncDeviceNotFound) {
		return nil, domain.ErrSyncSnapshotExpired
	}
	if snapshotError != nil {
		return nil, snapshotError
	}
	return BuildSyncChangePullResult(ctx, page, puller.signer)
}
