package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// errUploadMergeSessionPresent 表示本次复核仍能读取原会话，可安排后续重试。
var errUploadMergeSessionPresent = fmt.Errorf("原分片会话仍可读取: %w", domain.ErrUploadTaskNotReady)

// UploadMergeMissingSessionResult 只读结果中恰有一项，原对象已完成或本轮观察到两者均缺失。
type UploadMergeMissingSessionResult struct {
	Recovered *UploadMergeRecoveryObservation
	Missing   *domain.UploadMergeMissingSessionObservation
}

// CheckMissingSession 在存储报告会话缺失后，复核原会话、对象和当前租约。
func (preflight *UploadMergePreflightService) CheckMissingSession(ctx context.Context, claimed *domain.UploadTask, failure error) (*UploadMergeMissingSessionResult, error) {
	// 1. 确认会话缺失信号，按当前租约固定原存储目标。
	if !errors.Is(failure, domain.ErrUploadPartSessionMissing) {
		return nil, domain.ErrUploadTaskNotReady
	}
	target, err := loadUploadMergeTarget(ctx, claimed, preflight.recovery.getTarget)
	if err != nil {
		return nil, err
	}

	// 2. 先查原对象。对象缺失时，重新读取原会话。
	recovered, err := preflight.recovery.checkTarget(ctx, claimed, target)
	if err == nil {
		return &UploadMergeMissingSessionResult{Recovered: recovered}, nil
	}
	if !errors.Is(err, domain.ErrUploadObjectMissing) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, err = preflight.receipts.listTargetParts(ctx, claimed, target)
	if err == nil {
		return nil, errUploadMergeSessionPresent
	}
	if !errors.Is(err, domain.ErrUploadPartSessionMissing) {
		return nil, err
	}

	// 3. 原会话仍缺失时再查一次对象，并复核租约后返回结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	recovered, err = preflight.recovery.checkTarget(ctx, claimed, target)
	if err == nil {
		return &UploadMergeMissingSessionResult{Recovered: recovered}, nil
	}
	if !errors.Is(err, domain.ErrUploadObjectMissing) {
		return nil, err
	}
	return &UploadMergeMissingSessionResult{
		Missing: &domain.UploadMergeMissingSessionObservation{Task: *copyUploadTask(claimed), ObjectKey: target.ObjectKey},
	}, nil
}
