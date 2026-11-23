package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func tryUploadReuse(ctx context.Context, intent scopedUploadIntent, submission uploadSubmission) error {
	// 1. 首次提交复核源文件并保存待确认状态，之后只用原声明找回回执。
	if submission.Status == "PENDING_REUSE" {
		source, err := readSourceFileDigest(submission.SourcePath)
		if err != nil {
			return err
		}
		if source.SizeBytes != submission.Declaration.SizeBytes || source.SHA256 != submission.Declaration.SHA256 {
			return errors.New("源文件大小或摘要已变化，请重新选择")
		}
		if err := setUploadReuseStage(ctx, intent.db, intent, "PENDING_REUSE", "REUSE_PENDING"); err != nil {
			return err
		}
	}
	receipt, err := client.ReuseUploadContent(ctx, intent.owner.Address, intent.accessToken, submission.CreationKey, submission.Declaration)

	// 2. 确认未命中后先记录回退。普通创建的回执丢失时仍重试普通创建。
	if errors.Is(err, client.ErrReusableContentNotFound) {
		if saveError := setUploadReuseStage(ctx, intent.db, intent, "REUSE_PENDING", "PENDING_CREATE"); saveError != nil {
			return saveError
		}
		return err
	}
	if err != nil {
		return err
	}

	// 3. 将复用结果和已发布阶段一同保存，终态无需再发网络请求。
	if err := recordUploadReuse(ctx, intent, receipt); err != nil {
		return errors.New("无法保存文件复用回执，请重试同一条记录")
	}
	return nil
}

func setUploadReuseStage(ctx context.Context, writer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, intent scopedUploadIntent, previousStatus, status string) error {
	saveError := errors.New("无法保存文件复用状态，请重试同一条记录")
	result, err := writer.ExecContext(ctx, `UPDATE upload_intents SET status = ?
		WHERE id = ? AND server_address = ? AND user_id = ? AND status = ?`,
		status, intent.id, intent.owner.Address, intent.owner.UserID, previousStatus)
	if err != nil {
		return saveError
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return saveError
	}
	return nil
}

func recordUploadReuse(ctx context.Context, intent scopedUploadIntent, receipt client.UploadReuseReceipt) error {
	tx, err := intent.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setUploadReuseStage(ctx, tx, intent, "REUSE_PENDING", "REUSED"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO upload_reuse_results (intent_id, entry_id, name) VALUES (?, ?, ?)`,
		intent.id, receipt.EntryID, receipt.Name); err != nil {
		return err
	}
	return tx.Commit()
}
