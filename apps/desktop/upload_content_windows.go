package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

type uploadContentSource struct {
	target     client.SingleUploadContentTarget
	path       string
	sha256     string
	localState string
}

// SendContent 发送当前账号的已创建任务正文，并保存待查看校验结果的状态。
func (uploads *UploadIntents) SendContent(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.actOnUploadIntent(ctx, intentID, func(intent scopedUploadIntent) error {
		// 1. 读取持久化的任务地址和源文件，查询服务端是否已经收过正文。
		content, err := readUploadContentSource(ctx, intent)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("当前账号找不到已创建的上传任务")
		}
		if err != nil {
			return errors.New("无法读取本机正文记录")
		}
		if content.localState == "VERIFYING" {
			return nil
		}
		if content.localState != "CREATED" {
			return errors.New("当前本机任务无法发送正文")
		}
		status, err := client.ReadSingleUploadStatus(ctx, intent.owner.Address, intent.accessToken, content.target)
		if err != nil {
			return err
		}

		// 2. 只有远端仍接收正文时才复核源文件并发送。
		switch status {
		case "UPLOADING":
			source, err := openVerifiedUploadSource(content)
			if err != nil {
				return err
			}
			defer source.Close()
			if err := client.SendSingleUploadContent(ctx, intent.owner.Address, intent.accessToken, content.target, source); err != nil {
				return err
			}
		case "VERIFYING", "COMPLETED":
			// 前一次发送已经被服务端接收，直接恢复本机正文状态。
		default:
			return errors.New("远端任务当前不接收正文")
		}

		// 3. 按原账号及任务 ID 更新本机状态，保留第 17 步创建回执。
		result, err := intent.db.ExecContext(ctx, `UPDATE upload_intents SET status = 'VERIFYING'
			WHERE id = ? AND server_address = ? AND user_id = ? AND status IN ('CREATED', 'VERIFYING')
			AND EXISTS (SELECT 1 FROM single_upload_creations WHERE intent_id = upload_intents.id AND task_id = ?)`,
			intent.id, intent.owner.Address, intent.owner.UserID, content.target.TaskID)
		if err == nil {
			var updated int64
			updated, err = result.RowsAffected()
			if err == nil && updated == 1 {
				return nil
			}
		}
		return errors.New("无法保存正文接收状态，再次发送前会查询远端状态")
	})
}

func readUploadContentSource(ctx context.Context, intent scopedUploadIntent) (uploadContentSource, error) {
	var content uploadContentSource
	err := intent.db.QueryRowContext(ctx, `SELECT c.task_id, c.content_path, i.size_bytes, i.source_path, i.sha256, i.status
		FROM upload_intents i JOIN single_upload_creations c ON c.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ?`, intent.id, intent.owner.Address, intent.owner.UserID).
		Scan(&content.target.TaskID, &content.target.ContentPath, &content.target.SizeBytes, &content.path, &content.sha256, &content.localState)
	return content, err
}

func openVerifiedUploadSource(content uploadContentSource) (*os.File, error) {
	source, err := openSourceFile(content.path)
	if err != nil {
		return nil, err
	}
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != content.target.SizeBytes ||
		before.Size() > client.MaxSingleUploadBytes {
		source.Close()
		return nil, errors.New("源文件大小或类型已变化，请重新选择")
	}
	digest, err := digestSourceSnapshot(source, before)
	if err != nil {
		source.Close()
		return nil, err
	}
	if digest.SHA256 != content.sha256 {
		source.Close()
		return nil, errors.New("源文件摘要已变化，请重新选择")
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		source.Close()
		return nil, errors.New("无法重置源文件读取位置")
	}
	return source, nil
}
