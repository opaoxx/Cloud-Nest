package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
)

type uploadSubmission struct {
	Declaration client.UploadDeclaration
	CreationKey string
	Status      string
	SourcePath  string
}

// Submit 创建已有本机意图对应的上传任务，确认回执后更新本机记录。
func (uploads *UploadIntents) Submit(ctx context.Context, intentID string) (UploadIntentResult, error) {
	// 同一数据目录只能有一个创建提交，防止复用请求与回退上传交叉。
	if !filepath.IsAbs(uploads.databasePath) {
		return UploadIntentResult{}, errors.New("无法打开本机上传记录")
	}
	lock, err := openExclusiveFileLock(uploads.databasePath + ".submit.lock")
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return UploadIntentResult{}, errors.New("正在提交上传记录，请稍后重试")
	}
	if err != nil {
		return UploadIntentResult{}, errors.New("无法锁定本机上传记录")
	}
	defer lock.Close()

	return uploads.actOnUploadIntent(ctx, intentID, func(ctx context.Context, intent scopedUploadIntent) error {
		submission, err := readUploadSubmission(ctx, intent.db, intent.owner, intent.id)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("当前账号找不到这条上传记录")
		}
		if err != nil {
			return errors.New("无法读取本机上传声明")
		}

		// 1. 新记录先尝试复用，只有明确未命中时才允许普通创建。
		switch submission.Status {
		case "CREATED", "VERIFYING", "MULTIPART_CREATED", "REUSED":
			return nil
		case "PENDING_REUSE", "REUSE_PENDING":
			err := tryUploadReuse(ctx, intent, submission)
			if !errors.Is(err, client.ErrReusableContentNotFound) {
				return err
			}
		case "PENDING_CREATE":
		default:
			return errors.New("当前上传记录无法创建任务")
		}
		receipt, err := client.CreateUpload(ctx, intent.owner.Address, intent.accessToken, submission.CreationKey, submission.Declaration)
		if err != nil {
			return err
		}

		// 2. 在事务内持久化回执及状态。失败后仍可用原键找回任务。
		checkpointCtx := transferCheckpointContext(ctx)
		if receipt.Multipart != nil {
			err = recordMultipartUploadCreation(checkpointCtx, intent, receipt)
		} else {
			err = recordUploadCreation(checkpointCtx, intent.db, intent.owner, intent.id, client.SingleUploadCreation{
				TaskID: receipt.TaskID, ContentPath: receipt.ContentPath, ExpiresAt: receipt.ExpiresAt,
			})
		}
		if err != nil {
			return errors.New("无法保存上传创建回执，请重试同一条记录")
		}
		return nil
	})
}

type scopedUploadIntent struct {
	db          *sql.DB
	owner       uploadOwner
	id          int64
	accessToken string
}

func (uploads *UploadIntents) actOnUploadTransfer(ctx context.Context, intentID string, action func(context.Context, scopedUploadIntent) error) (UploadIntentResult, error) {
	// 正文、分片、完成和取消共享原有的分片锁，同一数据目录一次执行一个操作。
	if !filepath.IsAbs(uploads.databasePath) {
		return UploadIntentResult{}, errors.New("无法打开本机上传记录")
	}
	lock, err := openExclusiveFileLock(uploads.databasePath + ".parts.lock")
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return UploadIntentResult{}, errors.New("上传任务正在处理，请稍后重试")
	}
	if err != nil {
		return UploadIntentResult{}, errors.New("无法锁定本机上传任务")
	}
	defer lock.Close()
	return uploads.actOnUploadIntent(ctx, intentID, action)
}

func (uploads *UploadIntents) actOnUploadIntent(ctx context.Context, intentID string, action func(context.Context, scopedUploadIntent) error) (UploadIntentResult, error) {
	// 1. 用当前会话确认归属，读取已保存的创建参数和幂等键。
	id, err := strconv.ParseInt(intentID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != intentID {
		return UploadIntentResult{}, errors.New("无效的本机上传记录")
	}
	items, err := runDesktopTransfer(ctx, uploads.account.transfers, func(ctx context.Context) ([]UploadIntentView, error) {
		return uploads.runScopedUploadAction(ctx, id, action)
	})
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return UploadIntentResult{LoginRequired: err.Error()}, nil
	}
	return UploadIntentResult{Items: items}, err
}

func (uploads *UploadIntents) runScopedUploadAction(ctx context.Context, id int64, action func(context.Context, scopedUploadIntent) error) ([]UploadIntentView, error) {
	return readForAccount(ctx, uploads.account, func(address string, session client.NativeSession) ([]UploadIntentView, error) {
		owner, err := resolveUploadOwner(ctx, address, session)
		if err != nil {
			return nil, err
		}
		db, err := openUploadIntents(ctx, uploads.databasePath)
		if err != nil {
			return nil, errors.New("无法打开本机上传记录")
		}
		defer db.Close()
		// 2. 以已确认的账号作用域执行本次操作，再刷新这组本机记录。
		if err := action(ctx, scopedUploadIntent{db: db, owner: owner, id: id, accessToken: session.AccessToken}); err != nil {
			return nil, err
		}
		items, err := listUploadIntents(transferCheckpointContext(ctx), db, owner)
		if err != nil {
			return nil, errors.New("无法读取本机上传记录")
		}
		return items, nil
	})
}

func readUploadSubmission(ctx context.Context, db *sql.DB, owner uploadOwner, id int64) (uploadSubmission, error) {
	var submission uploadSubmission
	err := db.QueryRowContext(ctx, `SELECT parent_id, name, size_bytes, sha256, creation_key, status, source_path
		FROM upload_intents WHERE id = ? AND server_address = ? AND user_id = ?`, id, owner.Address, owner.UserID).
		Scan(&submission.Declaration.ParentID, &submission.Declaration.Name, &submission.Declaration.SizeBytes,
			&submission.Declaration.SHA256, &submission.CreationKey, &submission.Status, &submission.SourcePath)
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
