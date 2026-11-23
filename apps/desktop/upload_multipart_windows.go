package main

import (
	"context"
	"errors"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// MultipartPlanView 是窗口展示的计划摘要，字节数字段以十进制字符串交付。
type MultipartPlanView struct {
	PartCount           int   `json:"part_count"`
	ConfirmedParts      int   `json:"confirmed_parts"`
	PartSizeBytes       int64 `json:"part_size_bytes,string"`
	LastPartOffsetBytes int64 `json:"last_part_offset_bytes,string"`
	LastPartSizeBytes   int64 `json:"last_part_size_bytes,string"`
}

func recordMultipartUploadCreation(ctx context.Context, intent scopedUploadIntent, receipt client.UploadCreation) error {
	tx, err := intent.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 保存当前账号的任务 ID、期限和服务端给出的固定片长与片数。
	plan := receipt.Multipart
	expiresAt := receipt.ExpiresAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO multipart_upload_creations
		(intent_id, task_id, part_size_bytes, part_count, expires_at)
		SELECT id, ?, ?, ?, ? FROM upload_intents WHERE id = ? AND server_address = ? AND user_id = ?
		AND size_bytes = ? AND status IN ('PENDING_CREATE', 'MULTIPART_CREATED') ON CONFLICT(intent_id) DO NOTHING`,
		receipt.TaskID, plan.PartSizeBytes, plan.PartCount, expiresAt, intent.id, intent.owner.Address, intent.owner.UserID, plan.SizeBytes); err != nil {
		return err
	}

	// 2. 重复确认时核对同一份计划，已有计划冲突时保留原记录。
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM multipart_upload_creations m JOIN upload_intents i ON i.id = m.intent_id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND i.size_bytes = ?
		AND m.task_id = ? AND m.part_size_bytes = ? AND m.part_count = ? AND m.expires_at = ?
	)`, intent.id, intent.owner.Address, intent.owner.UserID, plan.SizeBytes,
		receipt.TaskID, plan.PartSizeBytes, plan.PartCount, expiresAt).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return errors.New("分片计划与本机记录不一致")
	}

	// 3. 回执和队列状态一起提交。此时已有计划，正文进度仍为零。
	if _, err := tx.ExecContext(ctx, `UPDATE upload_intents SET status = 'MULTIPART_CREATED'
		WHERE id = ? AND server_address = ? AND user_id = ? AND status = 'PENDING_CREATE'`,
		intent.id, intent.owner.Address, intent.owner.UserID); err != nil {
		return err
	}
	return tx.Commit()
}
