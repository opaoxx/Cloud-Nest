package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCommitUserMultipartCompletionIntegration(t *testing.T) {
	const partSize = 16 << 20
	const checksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	read := func(t *testing.T, ctx context.Context, store *Store) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	execute := func(t *testing.T, ctx context.Context, pool *sql.DB, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(t *testing.T, ctx context.Context, pool *sql.DB, store *Store, partCount int) (*domain.UploadTask, []domain.UploadPartReceipt) {
		t.Helper()
		fileSize := (partCount-1)*partSize + 1
		execute(t, ctx, pool, "UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
		execute(t, ctx, pool, "UPDATE stored_objects SET file_size=? WHERE id=23", fileSize)
		execute(t, ctx, pool, `UPDATE upload_tasks SET upload_mode='MULTIPART',status='UPLOADING',storage_upload_id='fixture+A/=',
			file_size=?,part_size=?,part_count=?,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner=NULL,lease_until=NULL,lease_version=4,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`,
			fileSize, partSize, partCount, fileSize)
		claimed, err := store.ClaimUserMultipartCompletionLease(ctx, actor, read(t, ctx, store), "completion-snapshot", service.ResolveMultipartUploadCompletionStatus)
		if err != nil || claimed == nil {
			t.Fatal("准备核对租约失败", err)
		}
		receipts := make([]domain.UploadPartReceipt, partCount)
		for index := range receipts {
			receipts[index] = domain.UploadPartReceipt{
				PartNumber: uint32(index + 1), SizeBytes: partSize, ETag: fmt.Sprintf("Fixture-ETag-%d", index+1), ChecksumSHA256: checksum,
			}
		}
		receipts[partCount-1].SizeBytes = 1
		return claimed, receipts
	}
	readParts := func(t *testing.T, ctx context.Context, store *Store) []models.UploadPart {
		t.Helper()
		var rows []models.UploadPart
		if err := store.database.WithContext(ctx).Where("task_id = ?", 19).Order("part_number ASC").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		return rows
	}

	t.Run("atomic snapshot and no overwrite", func(t *testing.T) {
		// 1. 跨三个插入批次，提交后读取任务和完整快照，重复请求不能改写已受理快照。
		ctx, pool, store := prepareMultipartCompletionDatabase(t)
		claimed, receipts := claim(t, ctx, pool, store, 1001)
		result, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts)
		if err != nil || result == nil {
			t.Fatal("完成快照提交失败", err)
		}
		expected := *claimed
		expected.Status = domain.UploadStatusCompleting
		expected.LeaseOwner, expected.LeaseUntil, expected.NextAttemptAt = nil, nil, nil
		expected.LastErrorCode, expected.LastErrorMessage, expected.UpdatedAt = nil, nil, result.UpdatedAt
		if !reflect.DeepEqual(result, &expected) || !reflect.DeepEqual(result, read(t, ctx, store)) {
			t.Fatal("完成状态、释放租约或任务字段不符")
		}
		parts := readParts(t, ctx, store)
		if len(parts) != len(receipts) {
			t.Fatal("未保存完整快照", len(parts))
		}
		for index, part := range parts {
			receipt := receipts[index]
			if part.TaskID != claimed.ID || part.PartNumber != receipt.PartNumber || part.SizeBytes != receipt.SizeBytes ||
				part.ETag != receipt.ETag || part.ChecksumAlgorithm == nil || *part.ChecksumAlgorithm != "SHA256" ||
				part.ChecksumValue == nil || *part.ChecksumValue != receipt.ChecksumSHA256 ||
				!part.ObservedAt.Equal(parts[0].ObservedAt) || part.ObservedAt.Before(claimed.UpdatedAt) || part.ObservedAt.After(result.UpdatedAt) {
				t.Fatal("回执或核对时间不符，分片编号", part.PartNumber)
			}
		}
		var used, reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage, reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil ||
			used != 0 || reserved != claimed.FileSize {
			t.Fatal("保存快照影响了配额", used, reserved, err)
		}
		receipts[0].ETag = "Different"
		if again, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts); again != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) ||
			!reflect.DeepEqual(parts, readParts(t, ctx, store)) || !reflect.DeepEqual(result, read(t, ctx, store)) {
			t.Fatal("重复提交应保留已受理快照", err)
		}
		t.Logf("保存 %d 行回执，阶段 %s，预占 %d 字节", len(parts), result.Status, reserved)
	})

	t.Run("stage failure rolls back inserted rows", func(t *testing.T) {
		// 2. 仅在独占库拦截阶段更新，这时批量插入已执行，事务应回滚所有回执。
		ctx, pool, store := prepareMultipartCompletionDatabase(t)
		claimed, receipts := claim(t, ctx, pool, store, 2)
		execute(t, ctx, pool, "ALTER TABLE upload_tasks ADD CONSTRAINT fixture_block_completing CHECK (status <> 'COMPLETING')")
		result, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts)
		var mysqlError *mysql.MySQLError
		if result != nil || err == nil || !errors.As(err, &mysqlError) || mysqlError.Number != 3819 ||
			!strings.Contains(err.Error(), "推进分片上传至完成阶段失败") {
			t.Fatal("预期隔离约束拦截阶段更新", err)
		}
		if rows := readParts(t, ctx, store); len(rows) != 0 || !reflect.DeepEqual(claimed, read(t, ctx, store)) {
			t.Fatal("阶段写入失败后未完整回滚", len(rows))
		}
		execute(t, ctx, pool, "ALTER TABLE upload_tasks DROP CHECK fixture_block_completing")
		if recovered, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts); err != nil ||
			recovered == nil || recovered.Status != domain.UploadStatusCompleting || len(readParts(t, ctx, store)) != 2 {
			t.Fatal("回滚后无法使用原租约重新提交", err)
		}
		t.Log("阶段写入失败后快照 0 行，原任务及租约保持一致，移除测试约束后提交成功")
	})

	for _, test := range []struct {
		name, change string
		want         error
	}{
		{"expired lease", "lease_until=UTC_TIMESTAMP(6)", domain.ErrUploadTaskBusy},
		{"stale version", "lease_version=6", domain.ErrUploadTaskBusy},
		{"changed session", "storage_upload_id='Fixture+A/='", domain.ErrUploadTaskBusy},
		{"expired task", "expires_at=UTC_TIMESTAMP(6)", domain.ErrUploadTaskNotReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, pool, store := prepareMultipartCompletionDatabase(t)
			claimed, receipts := claim(t, ctx, pool, store, 2)
			execute(t, ctx, pool, "UPDATE upload_tasks SET "+test.change+" WHERE id=19")
			before := read(t, ctx, store)
			if result, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts); result != nil || !errors.Is(err, test.want) ||
				len(readParts(t, ctx, store)) != 0 || !reflect.DeepEqual(before, read(t, ctx, store)) {
				t.Fatal("无效期限、租约或绑定改变了任务或回执", err)
			}
		})
	}
	t.Run("reject incomplete or existing snapshot", func(t *testing.T) {
		ctx, pool, store := prepareMultipartCompletionDatabase(t)
		claimed, receipts := claim(t, ctx, pool, store, 2)
		if result, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts[:1]); result != nil ||
			!errors.Is(err, domain.ErrInvalidUploadTaskData) || len(readParts(t, ctx, store)) != 0 {
			t.Fatal("不完整回执未被拒绝", err)
		}
		execute(t, ctx, pool, "INSERT INTO upload_parts(task_id,part_number,etag,size,observed_at) VALUES(19,3,'Unexpected',1,UTC_TIMESTAMP(6))")
		before := readParts(t, ctx, store)
		if result, err := store.CommitUserMultipartCompletion(ctx, actor, claimed, receipts); result != nil ||
			!errors.Is(err, domain.ErrInvalidUploadTaskData) || !reflect.DeepEqual(before, readParts(t, ctx, store)) ||
			!reflect.DeepEqual(claimed, read(t, ctx, store)) {
			t.Fatal("异常存量回执未被保护", err)
		}
	})
}
