package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

type multipartUploadSource struct {
	taskID         string
	parentID       string
	path           string
	sha256         string
	plan           client.MultipartPlan
	firstPartSaved bool
}

// SendFirstPart sends part 1 for an existing Multipart intent.
func (uploads *UploadIntents) SendFirstPart(ctx context.Context, intentID string) (UploadIntentResult, error) {
	if !uploads.partUploadMu.TryLock() {
		return UploadIntentResult{}, errors.New("正在上传分片，请稍后重试")
	}
	defer uploads.partUploadMu.Unlock()

	return uploads.actOnUploadIntent(ctx, intentID, func(intent scopedUploadIntent) error {
		// 1. 读取当前账号的计划与源文件，已保存回执时直接返回队列。
		upload, err := readMultipartUploadSource(ctx, intent)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("当前账号找不到分片上传任务")
		}
		if err != nil {
			return errors.New("无法读取本机分片上传记录")
		}
		if upload.firstPartSaved {
			return nil
		}
		partRange, err := upload.plan.PartRange(1)
		if err != nil {
			return err
		}
		if err := client.CheckMultipartUploadReady(ctx, intent.owner.Address, intent.accessToken,
			upload.taskID, upload.parentID, upload.plan.SizeBytes); err != nil {
			return err
		}
		source, err := openSourceFile(upload.path)
		if err != nil {
			return err
		}
		defer source.Close()
		digest, err := digestMultipartPart(ctx, source, upload, partRange)
		if err != nil {
			return err
		}

		// 2. 获取绑定到第一片摘要的签名，使用同一源文件句柄发送其字节范围。
		grant, err := client.IssueUploadPartURL(ctx, intent.owner.Address, intent.accessToken, upload.taskID, 1, partRange.SizeBytes, digest)
		if err != nil {
			return err
		}
		receipt, err := client.SendUploadPart(ctx, grant, io.NewSectionReader(source, partRange.OffsetBytes, partRange.SizeBytes))
		if err != nil {
			return err
		}

		// 3. 只有 SQLite 保存了回执，列表才增加已确认片数和字节。
		if err := recordMultipartPart(ctx, intent, upload.taskID, receipt); err != nil {
			return errors.New("无法保存分片回执，请重试同一片")
		}
		return nil
	})
}

func readMultipartUploadSource(ctx context.Context, intent scopedUploadIntent) (multipartUploadSource, error) {
	var upload multipartUploadSource
	err := intent.db.QueryRowContext(ctx, `SELECT m.task_id, i.parent_id, i.source_path, i.sha256,
		i.size_bytes, m.part_size_bytes, m.part_count,
		EXISTS (SELECT 1 FROM multipart_upload_parts WHERE intent_id = i.id AND part_number = 1)
		FROM upload_intents i JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND i.status = 'MULTIPART_CREATED'`,
		intent.id, intent.owner.Address, intent.owner.UserID).
		Scan(&upload.taskID, &upload.parentID, &upload.path, &upload.sha256, &upload.plan.SizeBytes,
			&upload.plan.PartSizeBytes, &upload.plan.PartCount, &upload.firstPartSaved)
	return upload, err
}

func digestMultipartPart(ctx context.Context, source *os.File, upload multipartUploadSource, partRange client.MultipartPartRange) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != upload.plan.SizeBytes {
		return empty, errors.New("源文件大小或类型已变化，请重新选择")
	}

	// 1. 在同一次顺序读取中计算整文件摘要，并给指定分片单独累计摘要。
	whole := sha256.New()
	part := sha256.New()
	stream := io.TeeReader(io.NewSectionReader(source, 0, before.Size()), whole)
	if _, err := io.CopyN(io.Discard, stream, partRange.OffsetBytes); err != nil {
		return empty, errors.New("无法完整读取源文件")
	}
	if _, err := io.CopyN(part, stream, partRange.SizeBytes); err != nil {
		return empty, errors.New("无法完整读取分片正文")
	}
	if _, err := io.CopyN(io.Discard, stream, before.Size()-partRange.OffsetBytes-partRange.SizeBytes); err != nil {
		return empty, errors.New("无法完整读取源文件")
	}

	// 2. 签发前复查原文件身份，存储端再用分片摘要验证实际收到的正文。
	after, err := source.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		hex.EncodeToString(whole.Sum(nil)) != upload.sha256 {
		return empty, errors.New("源文件摘要已变化，请重新选择")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return [sha256.Size]byte(part.Sum(nil)), nil
}

func recordMultipartPart(ctx context.Context, intent scopedUploadIntent, taskID string, receipt client.UploadPartReceipt) error {
	// 1. 只允许当前账号的原任务写入，重复确认同一片不会增加行数。
	if _, err := intent.db.ExecContext(ctx, `INSERT INTO multipart_upload_parts
		(intent_id, part_number, size_bytes, sha256, etag)
		SELECT i.id, ?, ?, ?, ? FROM upload_intents i JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND m.task_id = ?
		AND i.status = 'MULTIPART_CREATED' AND ? BETWEEN 1 AND m.part_count
		AND ? = min(m.part_size_bytes, i.size_bytes - (? - 1) * m.part_size_bytes)
		ON CONFLICT(intent_id, part_number) DO NOTHING`,
		receipt.Number, receipt.SizeBytes, receipt.SHA256, receipt.ETag,
		intent.id, intent.owner.Address, intent.owner.UserID, taskID, receipt.Number, receipt.SizeBytes, receipt.Number); err != nil {
		return err
	}

	// 2. 保留第一份确认回执。重复提交不同数据时报告冲突。
	var matches bool
	err := intent.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM multipart_upload_parts r
		JOIN upload_intents i ON i.id = r.intent_id JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND m.task_id = ?
		AND r.part_number = ? AND r.size_bytes = ? AND r.sha256 = ? AND r.etag = ?
	)`, intent.id, intent.owner.Address, intent.owner.UserID, taskID,
		receipt.Number, receipt.SizeBytes, receipt.SHA256, receipt.ETag).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("分片回执与本机记录不一致")
	}
	return nil
}
