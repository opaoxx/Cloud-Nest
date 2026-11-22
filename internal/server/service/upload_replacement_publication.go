package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// publishUploadReplacement 在已锁定账号、任务及原文件的事务内完成一次内容替换。
func publishUploadReplacement(
	ctx context.Context, txn UploadPublicationTransaction, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest,
) (*domain.UploadTask, error) {
	// 1. 选定已校验的发布对象。
	entryID, expectedRevision := *task.ReplacementEntryID, *task.ReplacementExpectedRevision
	selected, err := txn.SelectUploadPublicationObject(ctx, task, staged, digest)
	if err != nil {
		return nil, err
	}
	if selected == nil || selected.ID == 0 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 保存旧内容、替换当前引用、结算预占并记录完成版本。
	entry, err := txn.ApplyUploadReplacementContent(ctx, task, selected.ID, digest, domain.DefaultFileVersionRetentionDays)
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.ID != entryID || entry.Revision <= expectedRevision || entry.Revision-expectedRevision != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	completed, err := txn.CompleteUploadReplacement(ctx, task, selected.ID, entry)
	if err != nil {
		return nil, err
	}
	if completed == nil || completed.ID != task.ID || completed.UserID != task.UserID || completed.Status != domain.UploadStatusCompleted ||
		completed.PublishedObjectID == nil || *completed.PublishedObjectID != selected.ID ||
		completed.ResultEntryID == nil || *completed.ResultEntryID != entry.ID ||
		completed.ReplacementPublishedRevision == nil || *completed.ReplacementPublishedRevision != entry.Revision {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 保护实际发布对象，再追加变更。任一步失败交给外层事务回滚。
	if err := protectPublishedUploadObject(ctx, txn, task, selected.ID, entry.ID); err != nil {
		return nil, err
	}
	event, err := domain.NewFileContentReplacedChangeEvent(entry, digest)
	if err != nil {
		return nil, err
	}
	if _, err := txn.AppendUserChangeEvent(ctx, event); err != nil {
		return nil, err
	}
	return completed, nil
}
