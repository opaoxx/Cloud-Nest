package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// CheckStatus 查询当前账号上传任务的校验和发布结果。
func (uploads *UploadIntents) CheckStatus(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.actOnUploadTransfer(ctx, intentID, func(ctx context.Context, intent scopedUploadIntent) error {
		// 1. 按账号和本机记录 ID 读取远端任务。已有终态直接使用本机结果。
		content, err := readUploadContentSource(ctx, intent)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("当前账号找不到已创建的上传任务")
		}
		if err != nil {
			return errors.New("无法读取本机上传任务")
		}
		if content.localState != "CREATED" && content.localState != "VERIFYING" {
			return nil
		}

		// 2. 查询同一任务和目录。等待中的结果可继续查看，终态保存后可回顾。
		snapshot, err := client.ReadSingleUploadTask(ctx, intent.owner.Address, intent.accessToken, content.target, content.parentID)
		if err != nil {
			return err
		}
		switch snapshot.Status {
		case "VERIFYING", "CANCELLING", "COMPLETED", "FAILED", "CANCELLED", "EXPIRED":
			if err := recordUploadOutcome(transferCheckpointContext(ctx), intent, content.target.TaskID, snapshot); err != nil {
				return errors.New("无法保存上传结果，请重新查看")
			}
		}
		return nil
	})
}

func recordUploadOutcome(ctx context.Context, intent scopedUploadIntent, taskID string, snapshot client.SingleUploadTaskSnapshot) error {
	tx, err := intent.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 新状态或此前的本机待校验状态能证明完整正文已接收。
	if _, err := tx.ExecContext(ctx, `INSERT INTO single_upload_confirmations (intent_id, confirmed_bytes)
		SELECT i.id, i.size_bytes FROM upload_intents i JOIN single_upload_creations c ON c.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND c.task_id = ?
		AND i.status IN ('CREATED', 'VERIFYING', 'UPLOAD_CANCEL_PENDING', 'CANCELLING', ?)
		AND (i.status = 'VERIFYING' OR ? IN ('VERIFYING', 'COMPLETED'))
		ON CONFLICT(intent_id) DO NOTHING`,
		intent.id, intent.owner.Address, intent.owner.UserID, taskID, snapshot.Status, snapshot.Status); err != nil {
		return err
	}

	// 2. 只允许更新同账号的原任务，迟到的校验状态无法覆盖已保存的终态。
	result, err := tx.ExecContext(ctx, `UPDATE upload_intents SET status = ?
		WHERE id = ? AND server_address = ? AND user_id = ?
		AND (status IN ('CREATED', 'VERIFYING', ?) OR
			(status IN ('UPLOAD_CANCEL_PENDING', 'CANCELLING') AND ? IN ('CANCELLING', 'COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED')))
		AND EXISTS (SELECT 1 FROM single_upload_creations WHERE intent_id = upload_intents.id AND task_id = ?)`,
		snapshot.Status, intent.id, intent.owner.Address, intent.owner.UserID, snapshot.Status, snapshot.Status, taskID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errors.New("上传状态或归属已变化")
	}

	// 3. 进度、状态和最终结果一起提交，重复确认时保留第一份结果。
	if uploadTerminal(snapshot.Status) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO single_upload_outcomes (intent_id, result_entry_id, error_code, error_message)
			VALUES (?, ?, ?, ?) ON CONFLICT(intent_id) DO NOTHING`,
			intent.id, snapshot.ResultEntryID, snapshot.ErrorCode, snapshot.ErrorMessage); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func uploadTerminal(status string) bool {
	switch status {
	case "COMPLETED", "FAILED", "CANCELLED", "EXPIRED":
		return true
	default:
		return false
	}
}
