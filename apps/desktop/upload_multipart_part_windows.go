package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
)

const multipartParallelParts = 2

type multipartUploadSource struct {
	taskID   string
	parentID string
	path     string
	sha256   string
	plan     client.MultipartPlan
}

type multipartPartJob struct {
	declaration client.UploadPartDeclaration
	offsetBytes int64
}

// SendFirstPart sends part 1 for an existing Multipart intent.
func (uploads *UploadIntents) SendFirstPart(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.sendMultipartParts(ctx, intentID, true)
}

// SendRemainingParts uploads all parts that have no local receipt in bounded batches.
func (uploads *UploadIntents) SendRemainingParts(ctx context.Context, intentID string) (UploadIntentResult, error) {
	return uploads.sendMultipartParts(ctx, intentID, false)
}

func (uploads *UploadIntents) sendMultipartParts(ctx context.Context, intentID string, firstOnly bool) (UploadIntentResult, error) {
	if !filepath.IsAbs(uploads.databasePath) {
		return UploadIntentResult{}, errors.New("无法打开本机上传记录")
	}
	lock, err := openExclusiveFileLock(uploads.databasePath + ".parts.lock")
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return UploadIntentResult{}, errors.New("正在上传分片，请稍后重试")
	}
	if err != nil {
		return UploadIntentResult{}, errors.New("无法锁定本机分片上传队列")
	}
	defer lock.Close()

	return uploads.actOnUploadIntent(ctx, intentID, func(intent scopedUploadIntent) error {
		// 1. 读取当前账号的计划，用同一数据目录中的文件锁防止窗口间重复派发。
		upload, err := readMultipartUploadSource(ctx, intent)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("当前账号找不到分片上传任务")
		}
		if err != nil {
			return errors.New("无法读取本机分片上传记录")
		}
		lastNumber := upload.plan.PartCount
		if firstOnly {
			lastNumber = 1
		}
		numbers, err := pendingMultipartParts(ctx, intent, lastNumber)
		if err != nil || len(numbers) == 0 {
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
		jobs, err := digestMultipartParts(ctx, source, upload, numbers)
		if err != nil {
			return err
		}

		// 2. 一批最多两片，等待本批所有结果保存后才开始下一批。
		for len(jobs) > 0 {
			count := min(len(jobs), multipartParallelParts)
			if err := sendMultipartBatch(ctx, intent, upload.taskID, source, jobs[:count]); err != nil {
				return err
			}
			jobs = jobs[count:]
		}
		return nil
	})
}

func readMultipartUploadSource(ctx context.Context, intent scopedUploadIntent) (multipartUploadSource, error) {
	var upload multipartUploadSource
	err := intent.db.QueryRowContext(ctx, `SELECT m.task_id, i.parent_id, i.source_path, i.sha256,
		i.size_bytes, m.part_size_bytes, m.part_count
		FROM upload_intents i JOIN multipart_upload_creations m ON m.intent_id = i.id
		WHERE i.id = ? AND i.server_address = ? AND i.user_id = ? AND i.status = 'MULTIPART_CREATED'`,
		intent.id, intent.owner.Address, intent.owner.UserID).
		Scan(&upload.taskID, &upload.parentID, &upload.path, &upload.sha256, &upload.plan.SizeBytes,
			&upload.plan.PartSizeBytes, &upload.plan.PartCount)
	return upload, err
}

func pendingMultipartParts(ctx context.Context, intent scopedUploadIntent, lastNumber int) ([]int, error) {
	rows, err := intent.db.QueryContext(ctx, `SELECT part_number FROM multipart_upload_parts WHERE intent_id = ?`, intent.id)
	if err != nil {
		return nil, errors.New("无法读取本机已确认分片")
	}
	defer rows.Close()
	saved := make(map[int]bool)
	for rows.Next() {
		var number int
		if err := rows.Scan(&number); err != nil {
			return nil, errors.New("无法读取本机已确认分片")
		}
		saved[number] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("无法读取本机已确认分片")
	}
	var pending []int
	for number := 1; number <= lastNumber; number++ {
		if !saved[number] {
			pending = append(pending, number)
		}
	}
	return pending, nil
}

func digestMultipartParts(ctx context.Context, source *os.File, upload multipartUploadSource, numbers []int) ([]multipartPartJob, error) {
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != upload.plan.SizeBytes {
		return nil, errors.New("源文件大小或类型已变化，请重新选择")
	}

	// 1. 一次顺序扫描复核整文件，同时为待发字节段累计摘要，跳过段也计入整文件。
	whole := sha256.New()
	stream := io.TeeReader(io.NewSectionReader(source, 0, before.Size()), whole)
	jobs := make([]multipartPartJob, 0, len(numbers))
	var offset int64
	for _, number := range numbers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		partRange, err := upload.plan.PartRange(number)
		if err != nil {
			return nil, err
		}
		if _, err := io.CopyN(io.Discard, stream, partRange.OffsetBytes-offset); err != nil {
			return nil, errors.New("无法完整读取源文件")
		}
		part := sha256.New()
		if _, err := io.CopyN(part, stream, partRange.SizeBytes); err != nil {
			return nil, errors.New("无法完整读取分片正文")
		}
		jobs = append(jobs, multipartPartJob{offsetBytes: partRange.OffsetBytes, declaration: client.UploadPartDeclaration{
			Number: number, SizeBytes: partRange.SizeBytes, Digest: [sha256.Size]byte(part.Sum(nil)),
		}})
		offset = partRange.OffsetBytes + partRange.SizeBytes
	}
	if _, err := io.CopyN(io.Discard, stream, before.Size()-offset); err != nil {
		return nil, errors.New("无法完整读取源文件")
	}

	// 2. 签发前复查原文件身份，存储端再用分片摘要验证实际收到的正文。
	after, err := source.Stat()
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		hex.EncodeToString(whole.Sum(nil)) != upload.sha256 {
		return nil, errors.New("源文件摘要已变化，请重新选择")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func sendMultipartBatch(ctx context.Context, intent scopedUploadIntent, taskID string, source *os.File, jobs []multipartPartJob) error {
	// 1. 整批签名验证通过后，再为每个字节段启动独立的流式 PUT。
	declarations := make([]client.UploadPartDeclaration, len(jobs))
	for index, job := range jobs {
		declarations[index] = job.declaration
	}
	grants, err := client.IssueUploadPartURLs(ctx, intent.owner.Address, intent.accessToken, taskID, declarations)
	if err != nil {
		return err
	}
	type partResult struct {
		number  int
		receipt client.UploadPartReceipt
		err     error
	}
	results := make(chan partResult, len(jobs))
	for index, job := range jobs {
		go func() {
			part := io.NewSectionReader(source, job.offsetBytes, job.declaration.SizeBytes)
			receipt, err := client.SendUploadPart(ctx, grants[index], part)
			results <- partResult{number: job.declaration.Number, receipt: receipt, err: err}
		}()
	}

	// 2. 逐个消费结果并串行入库。即使一个结果失败，也保存同批成功的回执。
	var firstError error
	for range jobs {
		result := <-results
		if result.err == nil {
			if err := recordMultipartPart(ctx, intent, taskID, result.receipt); err != nil {
				result.err = errors.New("无法保存分片回执")
			}
		}
		if firstError == nil && result.err != nil {
			firstError = fmt.Errorf("第 %d 片未确认：%w", result.number, result.err)
		}
	}
	return firstError
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
