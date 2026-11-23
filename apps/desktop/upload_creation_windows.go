package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

type uploadSubmission struct {
	Declaration client.SingleUploadDeclaration
	CreationKey string
	Status      string
}

// Submit 创建已有本机意图对应的 SINGLE 任务，确认回执后更新本机记录。
func (uploads *UploadIntents) Submit(ctx context.Context, intentID string) (UploadIntentResult, error) {
	// 1. 用当前会话确认归属，读取已保存的创建参数和幂等键。
	id, err := strconv.ParseInt(intentID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != intentID {
		return UploadIntentResult{}, errors.New("无效的本机上传记录")
	}
	items, err := readForAccount(ctx, uploads.account, func(address string, session client.NativeSession) ([]UploadIntentView, error) {
		owner, err := resolveUploadOwner(ctx, address, session)
		if err != nil {
			return nil, err
		}
		db, err := openUploadIntents(ctx, uploads.databasePath)
		if err != nil {
			return nil, errors.New("无法打开本机上传记录")
		}
		defer db.Close()
		submission, err := readUploadSubmission(ctx, db, owner, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("当前账号找不到这条上传记录")
		}
		if err != nil {
			return nil, errors.New("无法读取本机上传声明")
		}

		// 2. 待提交记录才发送请求。令牌过期由会话层用原声明重试一次。
		switch submission.Status {
		case "PENDING_CREATE":
			receipt, err := client.CreateSingleUpload(ctx, address, session.AccessToken, submission.CreationKey, submission.Declaration)
			if err != nil {
				return nil, err
			}
			// 3. 在事务内持久化回执及状态。失败后仍可用原键找回任务。
			if err := recordUploadCreation(ctx, db, owner, id, receipt); err != nil {
				return nil, errors.New("无法保存上传创建回执，请重试同一条记录")
			}
		case "CREATED":
		default:
			return nil, errors.New("当前上传记录无法创建任务")
		}
		items, err := listUploadIntents(ctx, db, owner)
		if err != nil {
			return nil, errors.New("无法读取本机上传记录")
		}
		return items, nil
	})
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return UploadIntentResult{LoginRequired: err.Error()}, nil
	}
	return UploadIntentResult{Items: items}, err
}

func readUploadSubmission(ctx context.Context, db *sql.DB, owner uploadOwner, id int64) (uploadSubmission, error) {
	var submission uploadSubmission
	err := db.QueryRowContext(ctx, `SELECT parent_id, name, size_bytes, sha256, creation_key, status
		FROM upload_intents WHERE id = ? AND server_address = ? AND user_id = ?`, id, owner.Address, owner.UserID).
		Scan(&submission.Declaration.ParentID, &submission.Declaration.Name, &submission.Declaration.SizeBytes,
			&submission.Declaration.SHA256, &submission.CreationKey, &submission.Status)
	return submission, err
}

func recordUploadCreation(ctx context.Context, db *sql.DB, owner uploadOwner, id int64, receipt client.SingleUploadCreation) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 只允许当前记录所属的账号登记回执。
	expiresAt := receipt.ExpiresAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO single_upload_creations (intent_id, task_id, content_path, expires_at)
		SELECT id, ?, ?, ? FROM upload_intents WHERE id = ? AND server_address = ? AND user_id = ?
		AND status IN ('PENDING_CREATE', 'CREATED') ON CONFLICT(intent_id) DO NOTHING`,
		receipt.TaskID, receipt.ContentPath, expiresAt, id, owner.Address, owner.UserID); err != nil {
		return err
	}

	// 2. 并发确认必须得到同一份回执，已有不同回执时回滚。
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM single_upload_creations c JOIN upload_intents i ON i.id = c.intent_id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ?
		AND c.task_id = ? AND c.content_path = ? AND c.expires_at = ?
	)`, id, owner.Address, owner.UserID, receipt.TaskID, receipt.ContentPath, expiresAt).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return errors.New("创建回执与本机记录不一致")
	}

	// 3. 回执和已创建状态一起提交。
	if _, err := tx.ExecContext(ctx, `UPDATE upload_intents SET status = 'CREATED'
		WHERE id = ? AND server_address = ? AND user_id = ? AND status = 'PENDING_CREATE'`, id, owner.Address, owner.UserID); err != nil {
		return err
	}
	return tx.Commit()
}
