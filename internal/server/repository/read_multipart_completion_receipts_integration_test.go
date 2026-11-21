package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestReadMultipartCompletionReceiptsIntegration(t *testing.T) {
	const partSize = 16 << 20
	const checksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	expected := []domain.UploadPartReceipt{
		{PartNumber: 1, SizeBytes: partSize, ETag: "Fixture-First", ChecksumSHA256: checksum},
		{PartNumber: 2, SizeBytes: 1, ETag: "Fixture-Tail", ChecksumSHA256: checksum},
	}
	for _, fixture := range []struct {
		name, change string
		want         error
	}{
		{name: "complete ordered snapshot"},
		{"task stage changed", "UPDATE upload_tasks SET status='UPLOADING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"lease owner changed", "UPDATE upload_tasks SET lease_owner='MERGE-WORKER' WHERE id=19", domain.ErrUploadTaskBusy},
		{"stale version", "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", domain.ErrUploadTaskBusy},
		{"session changed", "UPDATE upload_tasks SET storage_upload_id='fixture+A/=' WHERE id=19", domain.ErrUploadTaskBusy},
		{"lease exactly expired", "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskBusy},
		{"task exactly expired", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", domain.ErrUploadTaskNotReady},
		{"declaration changed", "UPDATE upload_tasks SET expected_sha256=UNHEX(REPEAT('cd',32)) WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"missing tail", "DELETE FROM upload_parts WHERE task_id=19 AND part_number=2", domain.ErrInvalidUploadTaskData},
		{"gap with correct count", "UPDATE upload_parts SET part_number=3 WHERE task_id=19 AND part_number=2", domain.ErrInvalidUploadTaskData},
		{"extra row", "INSERT INTO upload_parts(task_id,part_number,etag,size,observed_at) VALUES (19,3,'extra',1,UTC_TIMESTAMP(6))", domain.ErrInvalidUploadTaskData},
		{"wrong tail size", "UPDATE upload_parts SET size=2 WHERE task_id=19 AND part_number=2", domain.ErrInvalidUploadTaskData},
		{"invalid etag", "UPDATE upload_parts SET etag='bad etag' WHERE task_id=19 AND part_number=1", domain.ErrInvalidUploadTaskData},
		{"missing checksum", "UPDATE upload_parts SET checksum_algorithm=NULL,checksum_value=NULL WHERE task_id=19 AND part_number=2", domain.ErrInvalidUploadTaskData},
		{"wrong checksum algorithm", "UPDATE upload_parts SET checksum_algorithm='SHA1' WHERE task_id=19 AND part_number=2", domain.ErrInvalidUploadTaskData},
		{"invalid SHA-256", "UPDATE upload_parts SET checksum_value='short' WHERE task_id=19 AND part_number=2", domain.ErrInvalidUploadTaskData},
		{"wrong account", "", domain.ErrUploadTaskNotFound},
		{"canceled read", "", context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 沿正常写入流程冻结回执，再领取真正的后台合并租约。
			ctx, pool, store := prepareMultipartCompletionDatabase(t)
			pool.SetMaxOpenConns(1)
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			var now int64
			if err := pool.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&now); err != nil {
				t.Fatal(err)
			}
			execute("SET timestamp = ?", now)
			execute("UPDATE users SET reserved_storage=? WHERE id=7", partSize+1)
			execute("UPDATE stored_objects SET file_size=? WHERE id=23", partSize+1)
			execute(`UPDATE upload_tasks SET upload_mode='MULTIPART',status='UPLOADING',storage_upload_id='Fixture+A/= ',
				file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				lease_owner=NULL,lease_until=NULL,lease_version=4,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`,
				partSize+1, partSize, partSize+1)
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			verification, err := store.ClaimUserMultipartCompletionLease(ctx, actor, readTask(), "verify-worker", service.ResolveMultipartUploadCompletionStatus)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitUserMultipartCompletion(ctx, actor, verification, expected); err != nil {
				t.Fatal(err)
			}
			candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 {
				t.Fatal("合成任务未进入后台候选", candidates, err)
			}
			claimed, err := store.ClaimMultipartUploadCompletionLease(ctx, candidates[0], "merge-worker")
			if err != nil {
				t.Fatal(err)
			}

			// 2. 逐项模拟扫描后变化，覆盖现有版本与期限的检查。
			if fixture.change != "" {
				execute(fixture.change)
			}
			readContext := ctx
			if fixture.name == "wrong account" {
				claimed.UserID = 8
			}
			if fixture.name == "canceled read" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				readContext = canceled
			}
			readRows := func() []models.UploadPart {
				t.Helper()
				var rows []models.UploadPart
				if err := store.database.WithContext(ctx).Where("task_id = ?", 19).Order("part_number ASC").Find(&rows).Error; err != nil {
					t.Fatal(err)
				}
				return rows
			}
			before, rowsBefore := readTask(), readRows()
			receipts, err := store.ReadMultipartCompletionReceipts(readContext, claimed)
			if !errors.Is(err, fixture.want) {
				t.Fatalf("预期错误 %v，实际 %v", fixture.want, err)
			}
			if fixture.want == nil {
				if !reflect.DeepEqual(receipts, expected) {
					t.Fatal("冻结回执的顺序或原值不符", receipts)
				}
				receipts[0].ETag = "changed-by-caller"
				again, err := store.ReadMultipartCompletionReceipts(ctx, claimed)
				if err != nil || !reflect.DeepEqual(again, expected) {
					t.Fatal("重复读取未保留原回执", again, err)
				}
			} else if receipts != nil {
				t.Fatal("失败时返回了部分回执", receipts)
			}

			// 3. 回查任务、回执和配额，证明这一步只读取。
			if !reflect.DeepEqual(before, readTask()) || !reflect.DeepEqual(rowsBefore, readRows()) {
				t.Fatal("读取操作改变了任务或回执")
			}
			var reserved, used uint64
			if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil ||
				reserved != partSize+1 || used != 0 {
				t.Fatal("读取操作改变了账号空间", reserved, used, err)
			}
		})
	}
}
