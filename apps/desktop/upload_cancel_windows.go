package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

const uploadCancelPending = "UPLOAD_CANCEL_PENDING"

// CancelUpload 提交取消，若此前已有请求则先核对原任务。
func (uploads *UploadIntents) CancelUpload(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.handleUploadCancellation(ctx, intentID, true)
}

// CheckUploadCancellation 只查询原任务，供取消受理或未确认后查看。
func (uploads *UploadIntents) CheckUploadCancellation(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.handleUploadCancellation(ctx, intentID, false)
}

func (uploads *UploadIntents) handleUploadCancellation(ctx context.Context, intentID string, submit bool) (UploadIntentResult, error) {
	return uploads.actOnUploadTransfer(ctx, intentID, func(ctx context.Context, intent scopedUploadIntent) error {
		// 1. 只使用当前账号已保存的原任务，不需要原文件或分片签名。
		target, state, err := readUploadCancellationTarget(ctx, intent)
		if err != nil {
			return err
		}
		if uploadTerminal(state) {
			return nil
		}
		writeSnapshot := func(snapshot client.UploadTaskSnapshot) error {
			record := recordUploadOutcome
			if target.Mode == "MULTIPART" {
				record = recordMultipartOutcome
			}
			checkpointCtx := ctx
			if snapshot.Status != uploadCancelPending {
				checkpointCtx = transferCheckpointContext(ctx)
			}
			if err := record(checkpointCtx, intent, target.TaskID, snapshot); err != nil {
				return errors.New("无法保存取消状态，请查看原任务的取消结果")
			}
			return nil
		}
		check := func() (bool, error) {
			snapshot, err := client.ReadUploadTask(ctx, intent.owner.Address, intent.accessToken, target)
			if err != nil {
				return false, err
			}
			if snapshot.Status == "CANCELLING" || uploadTerminal(snapshot.Status) {
				return true, writeSnapshot(snapshot)
			}
			return false, nil
		}

		// 2. 已有取消请求时先 GET，只有仍未受理且用户主动重试才再次 POST。
		if !submit || state == uploadCancelPending || state == "CANCELLING" {
			confirmed, err := check()
			if err != nil || confirmed || !submit || state == "CANCELLING" {
				return err
			}
		} else if err := writeSnapshot(client.UploadTaskSnapshot{Status: uploadCancelPending}); err != nil {
			return err
		}

		// 3. 保存服务端快照。回执有疑问时只查原任务，保留未确认状态供下次查看。
		snapshot, submitError := client.CancelUploadTask(ctx, intent.owner.Address, intent.accessToken, target)
		if submitError == nil {
			return writeSnapshot(snapshot)
		}
		if errors.Is(submitError, client.ErrAccessTokenExpired) {
			return submitError
		}
		if confirmed, err := check(); confirmed {
			return err
		}
		return submitError
	})
}

func readUploadCancellationTarget(ctx context.Context, intent scopedUploadIntent) (client.UploadTaskTarget, string, error) {
	var target client.UploadTaskTarget
	var state string
	err := intent.db.QueryRowContext(ctx, `SELECT COALESCE(c.task_id, m.task_id), i.parent_id, i.size_bytes, i.status,
		CASE WHEN c.task_id IS NOT NULL THEN 'SINGLE' ELSE 'MULTIPART' END
		FROM upload_intents i
		LEFT JOIN single_upload_creations c ON c.intent_id = i.id
		LEFT JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ?
		AND (c.intent_id IS NOT NULL OR m.intent_id IS NOT NULL)`,
		intent.id, intent.owner.Address, intent.owner.UserID).
		Scan(&target.TaskID, &target.ParentID, &target.SizeBytes, &state, &target.Mode)
	if errors.Is(err, sql.ErrNoRows) {
		return target, "", errors.New("当前账号找不到已创建的上传任务")
	}
	if err != nil {
		return target, "", errors.New("无法读取本机取消任务")
	}
	return target, state, nil
}
