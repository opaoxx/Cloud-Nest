package main

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// PauseMultipart 持久化暂停请求，正在处理的分片批次仍可完成。
func (uploads *UploadIntents) PauseMultipart(ctx context.Context, intentID string) (UploadIntentResult, error) {
	// 1. 核对当前账号，但跳过派发锁，让暂停可在上传进行时登记。
	return uploads.actOnUploadIntent(ctx, intentID, func(ctx context.Context, intent scopedUploadIntent) error {
		pauseID, err := uuid.NewRandom()
		if err != nil {
			return errors.New("无法生成暂停请求，请重试")
		}
		return recordMultipartPause(ctx, intent, pauseID.String())
	})
}

// ContinueMultipart 核对源文件，并从当前暂停请求对应的断点继续上传。
func (uploads *UploadIntents) ContinueMultipart(ctx context.Context, intentID, pauseID string) (UploadIntentResult, error) {
	if pauseID == "" {
		return UploadIntentResult{}, errors.New("请刷新后选择一项已暂停任务")
	}
	return uploads.sendMultipartParts(ctx, intentID, false, pauseID)
}

func recordMultipartPause(ctx context.Context, intent scopedUploadIntent, pauseID string) error {
	// 1. 只暂停当前账号尚可派发的分片任务。新请求替换旧标识，防止准备中的继续动作清掉它。
	result, err := intent.db.ExecContext(ctx, `INSERT INTO multipart_upload_pauses (intent_id, pause_id)
		SELECT i.id, ? FROM upload_intents i JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND i.status = 'MULTIPART_CREATED'
		ON CONFLICT(intent_id) DO UPDATE SET pause_id = excluded.pause_id`,
		pauseID, intent.id, intent.owner.Address, intent.owner.UserID)
	if err != nil {
		return errors.New("无法保存分片暂停请求，请重试")
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return errors.New("当前账号找不到可暂停的分片任务")
	}
	return nil
}

func clearMultipartPause(ctx context.Context, intent scopedUploadIntent, pauseID string) (bool, error) {
	// 1. 源和存储回执已核对。只清除发起继续时看到的暂停请求。
	result, err := intent.db.ExecContext(ctx, `DELETE FROM multipart_upload_pauses
		WHERE intent_id = ? AND pause_id = ? AND EXISTS (
			SELECT 1 FROM upload_intents i WHERE i.id = multipart_upload_pauses.intent_id
			AND i.server_address = ? AND i.user_id = ? AND i.status = 'MULTIPART_CREATED'
		)`, intent.id, pauseID, intent.owner.Address, intent.owner.UserID)
	if err != nil {
		return false, errors.New("无法继续已暂停的分片任务，请重试")
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func multipartPauseRequested(ctx context.Context, intent scopedUploadIntent) (bool, error) {
	var paused bool
	err := intent.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM multipart_upload_pauses WHERE intent_id = ?
	)`, intent.id).Scan(&paused)
	if err != nil {
		return false, errors.New("无法确认分片暂停状态")
	}
	return paused, nil
}
