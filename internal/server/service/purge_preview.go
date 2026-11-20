package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgePreviewReader 提供同一快照中的目标、父链和子树读取能力。
type PurgePreviewReader interface {
	FileEntryPathReader
	ListUserPurgePreviewSubtree(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error)
}

// PurgePreviewService 组合永久清理预检所需的只读查询和凭证签发。
type PurgePreviewService struct {
	// withSnapshot 同步执行一次读取回调，成功结束快照后返回 nil。
	withSnapshot func(context.Context, func(context.Context, PurgePreviewReader) error) error
	signer       *PurgePreviewSigner
	now          func() time.Time
}

// NewPurgePreviewService 绑定已有快照入口、签发器和应用时钟。
func NewPurgePreviewService(
	withSnapshot func(context.Context, func(context.Context, PurgePreviewReader) error) error,
	signer *PurgePreviewSigner,
	now func() time.Time,
) *PurgePreviewService {
	// 1. 检查并保存预检所需的运行依赖。
	if withSnapshot == nil || signer == nil || now == nil {
		panic("永久清理预检需要只读快照方法、签发器和时钟")
	}
	return &PurgePreviewService{withSnapshot: withSnapshot, signer: signer, now: now}
}

// Preview 为当前用户的顶层回收项生成清理预检结果。
func (previewService *PurgePreviewService) Preview(ctx context.Context, actor domain.Actor, entryID uint64) (*domain.PurgePreview, error) {
	// 1. 检查认证身份和目标 ID，进入只读快照。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 {
		return nil, fmt.Errorf("永久清理预检需要上下文、完整认证身份及非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var prepared *domain.PurgePreview
	var fingerprint [sha256.Size]byte
	snapshotError := previewService.withSnapshot(ctx, func(snapshotContext context.Context, reader PurgePreviewReader) error {
		// 2. 校验目标与父链，读取同一快照中的完整子树。
		target, err := ValidatePurgePreviewTarget(snapshotContext, actor, entryID, reader)
		if err != nil {
			return err
		}
		entries, readError := reader.ListUserPurgePreviewSubtree(snapshotContext, target.UserID, target.ID)
		if err := snapshotContext.Err(); err != nil {
			return err
		}
		if readError != nil {
			return fmt.Errorf("读取预检子树失败: %w", readError)
		}

		// 3. 汇总范围，核对目标版本并计算子树指纹。
		summary, err := SummarizePurgePreviewSubtree(entries)
		if err != nil {
			return err
		}
		fingerprint, err = buildPurgePreviewTargetFingerprint(target, entries)
		if err != nil {
			return err
		}
		prepared = &domain.PurgePreview{EntryID: target.ID, Revision: target.Revision, Summary: summary}
		return snapshotContext.Err()
	})

	// 4. 确认快照成功结束，再签发凭证并返回结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取永久清理预检快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	credential, err := previewService.signer.Sign(actor.UserID, prepared.EntryID, fingerprint, previewService.now())
	if err != nil {
		return nil, err
	}
	prepared.Credential = credential
	return prepared, nil
}
