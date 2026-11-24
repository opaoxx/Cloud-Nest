package main

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func reconcileMultipartParts(ctx context.Context, intent scopedUploadIntent, upload multipartUploadSource, jobs []multipartPartJob) ([]multipartPartJob, error) {
	// 1. 源文件已通过整文件校验。查询同一任务的完整存储回执。
	remote, err := client.ReadUploadParts(ctx, intent.owner.Address, intent.accessToken, upload.taskID, upload.plan)
	if err != nil {
		return nil, err
	}

	// 2. 以源文件的范围摘要核对实际回执，匹配时沿用存储返回的 ETag。
	confirmed, pending := compareMultipartParts(jobs, remote)
	if err := replaceMultipartPartReceipts(ctx, intent, upload.taskID, confirmed); err != nil {
		return nil, errors.New("无法保存分片对账结果，请稍后重试")
	}
	return pending, nil
}

func compareMultipartParts(jobs []multipartPartJob, remote []client.UploadPartReceipt) ([]client.UploadPartReceipt, []multipartPartJob) {
	byNumber := make(map[int]client.UploadPartReceipt, len(remote))
	for _, receipt := range remote {
		byNumber[receipt.Number] = receipt
	}
	var confirmed []client.UploadPartReceipt
	var pending []multipartPartJob
	for _, job := range jobs {
		receipt, exists := byNumber[job.declaration.Number]
		if exists && receipt.SizeBytes == job.declaration.SizeBytes && receipt.SHA256 == hex.EncodeToString(job.declaration.Digest[:]) {
			confirmed = append(confirmed, receipt)
		} else {
			pending = append(pending, job)
		}
	}
	return confirmed, pending
}

func replaceMultipartPartReceipts(ctx context.Context, intent scopedUploadIntent, taskID string, confirmed []client.UploadPartReceipt) error {
	tx, err := intent.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 核对本机范围和同一个远端任务，避免更新其他账号或其他阶段。
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM upload_intents i JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND m.task_id = ? AND i.status = 'MULTIPART_CREATED'
	)`, intent.id, intent.owner.Address, intent.owner.UserID, taskID).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return errors.New("本机分片任务已变化")
	}

	// 2. 在同一事务去掉失效的本机回执，保存本次核对通过的实际回执。
	if _, err := tx.ExecContext(ctx, `DELETE FROM multipart_upload_parts WHERE intent_id = ?`, intent.id); err != nil {
		return err
	}
	insert, err := tx.PrepareContext(ctx, `INSERT INTO multipart_upload_parts
		(intent_id, part_number, size_bytes, sha256, etag) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, receipt := range confirmed {
		if _, err := insert.ExecContext(ctx, intent.id, receipt.Number, receipt.SizeBytes, receipt.SHA256, receipt.ETag); err != nil {
			return err
		}
	}
	return tx.Commit()
}
