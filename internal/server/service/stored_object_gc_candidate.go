package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// StoredObjectGCCandidateProcessor 领取并删除单个到期候选。
type StoredObjectGCCandidateProcessor struct {
	// claim 提交领取事务后返回当前 DELETING 对象。
	claim func(context.Context, domain.StoredObjectGCCandidate) (*domain.StoredObject, error)
	// deleteObject 执行存储删除并提交完成状态。
	deleteObject func(context.Context, *domain.StoredObject) (*domain.StoredObject, error)
}

// NewStoredObjectGCCandidateProcessor 绑定领取和单项删除入口。
func NewStoredObjectGCCandidateProcessor(
	claim func(context.Context, domain.StoredObjectGCCandidate) (*domain.StoredObject, error),
	deleteObject func(context.Context, *domain.StoredObject) (*domain.StoredObject, error),
) *StoredObjectGCCandidateProcessor {
	// 1. 检查并保存执行依赖。
	if claim == nil || deleteObject == nil {
		panic("处理对象回收候选需要领取和删除方法")
	}
	return &StoredObjectGCCandidateProcessor{claim: claim, deleteObject: deleteObject}
}

// Process 领取一条到期候选，返回已提交的 DELETED 对象或本次错误。
func (processor *StoredObjectGCCandidateProcessor) Process(ctx context.Context, candidate domain.StoredObjectGCCandidate) (*domain.StoredObject, error) {
	// 1. 校验上下文和扫描候选。
	if ctx == nil {
		return nil, errors.New("处理对象回收候选需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !candidate.IsValid() {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 领取候选，核对已提交的 DELETING 记录。
	deleting, err := processor.claim(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if deleting == nil || deleting.ID != candidate.ObjectID || deleting.OwnerUserID != candidate.OwnerUserID ||
		deleting.Status != domain.StoredObjectStatusDeleting || deleting.UnreferencedAt == nil || deleting.GCAfter == nil ||
		!deleting.UnreferencedAt.Equal(candidate.UnreferencedAt) || !deleting.GCAfter.Equal(candidate.GCAfter) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 在领取事务结束后执行单项删除与写回。
	// 取消或删除失败时，已提交的 DELETING 记录可由恢复扫描接手。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return processor.deleteObject(ctx, deleting)
}
