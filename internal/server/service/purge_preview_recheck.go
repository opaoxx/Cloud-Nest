package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecheckPurgePreview 在同一已认证用户事务中复查首次确认的范围。
// reader 必须来自外层持有用户锁的事务。成功返回当前目标，供同一事务继续受理。
func RecheckPurgePreview(
	ctx context.Context,
	actor domain.Actor,
	entryID uint64,
	digest string,
	reader PurgePreviewReader,
	verifier *PurgePreviewVerifier,
	now func() time.Time,
) (*domain.FileEntry, error) {
	// 1. 在当前用户事务中检查目标与原父链。
	if verifier == nil || now == nil {
		return nil, fmt.Errorf("复查永久清理预检需要凭证校验器和时钟")
	}
	target, err := ValidatePurgePreviewTarget(ctx, actor, entryID, reader)
	if err != nil {
		return nil, err
	}

	// 2. 验证凭证的签名、归属及有效期。
	expectedFingerprint, err := verifier.Verify(target.UserID, target.ID, digest, now())
	if errors.Is(err, domain.ErrPurgePreviewExpired) {
		return nil, domain.ErrPurgePreviewChanged
	}
	if err != nil {
		return nil, err
	}

	// 3. 读取当前子树并重算指纹，比较后返回目标。
	entries, readError := reader.ListUserPurgePreviewSubtree(ctx, target.UserID, target.ID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(readError, domain.ErrFileEntrySubtreeTooLarge) {
		return nil, domain.ErrPurgePreviewChanged
	}
	if errors.Is(readError, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if readError != nil {
		return nil, fmt.Errorf("复查永久清理子树失败: %w", readError)
	}
	if len(entries) > domain.MaxFileEntrySubtreeNodes {
		return nil, domain.ErrPurgePreviewChanged
	}
	currentFingerprint, err := buildPurgePreviewTargetFingerprint(target, entries)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if currentFingerprint != expectedFingerprint {
		return nil, domain.ErrPurgePreviewChanged
	}
	return target, nil
}
