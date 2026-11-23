package main

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

const multipartCompletionPending = "MULTIPART_COMPLETION_PENDING"

// CompleteMultipart submits the saved part receipts after checking the original task.
func (uploads *UploadIntents) CompleteMultipart(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.finishMultipart(ctx, intentID, true)
}

// CheckMultipartStatus reads the original task without submitting completion.
func (uploads *UploadIntents) CheckMultipartStatus(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.finishMultipart(ctx, intentID, false)
}

func (uploads *UploadIntents) finishMultipart(ctx context.Context, intentID string, submit bool) (UploadIntentResult, error) {
	return uploads.actOnMultipartUpload(ctx, intentID, func(intent scopedUploadIntent) error {
		// 1. 先查询原任务。已受理或已进入终态时直接保存观察结果。
		upload, err := readMultipartUploadSource(ctx, intent)
		if err != nil {
			return errors.New("无法读取当前账号的本机分片任务")
		}
		if multipartTerminal(upload.status) {
			return nil
		}
		readStatus := func() (client.UploadTaskSnapshot, error) {
			return client.ReadMultipartUploadTask(ctx, intent.owner.Address, intent.accessToken,
				upload.taskID, upload.parentID, upload.plan.SizeBytes)
		}
		snapshot, err := readStatus()
		if err != nil {
			return err
		}
		if snapshot.Status != "UPLOADING" || !submit {
			return recordMultipartOutcome(ctx, intent, upload.taskID, snapshot)
		}

		// 2. 本机记录先进入待确认。即使部署重启或响应丢失，也能查询同一任务。
		receipts, err := readMultipartCompletionReceipts(ctx, intent, upload.plan.PartCount)
		if err != nil {
			return err
		}
		if err := recordMultipartOutcome(ctx, intent, upload.taskID, client.UploadTaskSnapshot{Status: multipartCompletionPending}); err != nil {
			return err
		}
		snapshot, submitError := client.CompleteMultipartUpload(ctx, intent.owner.Address, intent.accessToken,
			upload.taskID, upload.parentID, upload.plan, receipts)
		if submitError == nil {
			return recordMultipartOutcome(ctx, intent, upload.taskID, snapshot)
		}
		if errors.Is(submitError, client.ErrAccessTokenExpired) {
			return submitError
		}

		// 3. 完成响应不可用时按原任务对账，本次操作不重复发送 POST。
		snapshot, err = readStatus()
		if err != nil || (snapshot.Status == "UPLOADING" && errors.Is(submitError, client.ErrMultipartCompletionUnknown)) {
			return submitError
		}
		if err := recordMultipartOutcome(ctx, intent, upload.taskID, snapshot); err != nil {
			return err
		}
		if snapshot.Status == "COMPLETING" || snapshot.Status == "VERIFYING" || multipartTerminal(snapshot.Status) {
			return nil
		}
		return submitError
	})
}

func readMultipartCompletionReceipts(ctx context.Context, intent scopedUploadIntent, count int) ([]client.UploadPartReceipt, error) {
	rows, err := intent.db.QueryContext(ctx, `SELECT part_number, size_bytes, etag FROM multipart_upload_parts
		WHERE intent_id = ? ORDER BY part_number`, intent.id)
	if err != nil {
		return nil, errors.New("无法读取本机分片回执")
	}
	defer rows.Close()
	receipts := make([]client.UploadPartReceipt, 0, count)
	for rows.Next() {
		var receipt client.UploadPartReceipt
		if err := rows.Scan(&receipt.Number, &receipt.SizeBytes, &receipt.ETag); err != nil {
			return nil, errors.New("无法读取本机分片回执")
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("无法读取本机分片回执")
	}
	if len(receipts) != count {
		return nil, errors.New("分片回执不完整，请先核对并续传")
	}
	return receipts, nil
}

func multipartTerminal(status string) bool {
	switch status {
	case "COMPLETED", "FAILED", "CANCELLED", "EXPIRED":
		return true
	default:
		return false
	}
}

func recordMultipartOutcome(ctx context.Context, intent scopedUploadIntent, taskID string, snapshot client.UploadTaskSnapshot) error {
	status := snapshot.Status
	switch status {
	case "UPLOADING":
		status = "MULTIPART_CREATED"
	case "CREATING", "CANCELLING", "EXPIRING":
		return nil
	}
	writeError := errors.New("无法保存分片发布状态，请查看原任务的发布结果")
	tx, err := intent.db.BeginTx(ctx, nil)
	if err != nil {
		return writeError
	}
	defer tx.Rollback()

	// 1. 只更新当前账号的同一任务，已公布的终态不被旧状态覆盖。
	result, err := tx.ExecContext(ctx, `UPDATE upload_intents SET status = ?
		WHERE id = ? AND server_address = ? AND user_id = ?
		AND status IN ('MULTIPART_CREATED', 'MULTIPART_COMPLETION_PENDING', 'COMPLETING', 'VERIFYING')
		AND EXISTS (SELECT 1 FROM multipart_upload_creations WHERE intent_id = upload_intents.id AND task_id = ?)`,
		status, intent.id, intent.owner.Address, intent.owner.UserID, taskID)
	if err != nil {
		return writeError
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return writeError
	}

	// 2. 终态与发布条目或失败原因一起持久化。
	if multipartTerminal(status) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO multipart_upload_outcomes (intent_id, result_entry_id, error_code, error_message)
			VALUES (?, ?, ?, ?)`, intent.id, snapshot.ResultEntryID, snapshot.ErrorCode, snapshot.ErrorMessage); err != nil {
			return writeError
		}
	}
	if err := tx.Commit(); err != nil {
		return writeError
	}
	return nil
}
