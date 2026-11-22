package service

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PreparedSyncSnapshot 保存后续账号事务所需的信息和本次缓存的撤回凭据。
type PreparedSyncSnapshot struct {
	Binding   SyncSnapshotCacheBinding
	Header    domain.SyncSnapshotHeader
	ExpiresAt time.Time

	reservation *SyncSnapshotReservation
}

// SyncSnapshotPreparation 组合容量准入、一致性读取和临时缓存写入。
type SyncSnapshotPreparation struct {
	build func(context.Context, domain.Actor) (*BuiltSyncSnapshot, error)
	cache *SyncSnapshotCache
}

// NewSyncSnapshotPreparation 绑定快照构建方法与进程共享缓存。
func NewSyncSnapshotPreparation(
	build func(context.Context, domain.Actor) (*BuiltSyncSnapshot, error),
	cache *SyncSnapshotCache,
) *SyncSnapshotPreparation {
	if build == nil || cache == nil {
		panic("准备同步快照需要构建方法和共享缓存")
	}
	return &SyncSnapshotPreparation{build: build, cache: cache}
}

// Prepare 将一致性快照写入缓存，交由调用方在账号事务中提交新代次。
// 如果调用方确认事务未提交，可用结果中的绑定和预留撤回本次缓存。
func (preparation *SyncSnapshotPreparation) Prepare(ctx context.Context, actor domain.Actor, pageSize int) (*PreparedSyncSnapshot, error) {
	// 1. 校验身份和页大小，预留构建预算。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("准备同步快照需要上下文、完整认证身份和 1～100 的页大小")
	}
	reservation, err := preparation.cache.ReserveBuild(ctx)
	if err != nil {
		return nil, err
	}
	defer reservation.Release()

	// 2. 构建只读快照，生成标识和代次。
	snapshot, buildError := preparation.build(ctx, actor)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if buildError != nil {
		return nil, buildError
	}
	snapshotID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("生成同步快照 ID 失败: %w", err)
	}
	var generation domain.SyncCursorGeneration
	rand.Read(generation[:])
	binding := SyncSnapshotCacheBinding{
		SnapshotID: snapshotID, UserID: actor.UserID, DeviceID: actor.DeviceID,
		Generation: generation, PageSize: pageSize,
	}

	// 3. 写入缓存，交付后续事务所需的数据。
	expiresAt, err := preparation.cache.Put(ctx, reservation, binding, snapshot)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		preparation.cache.DiscardUncommitted(binding, reservation)
		return nil, err
	}
	return &PreparedSyncSnapshot{
		Binding: binding, Header: snapshot.Header, ExpiresAt: expiresAt, reservation: reservation,
	}, nil
}
