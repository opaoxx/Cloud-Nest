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

func TestBeginMultipartUploadVerificationIntegration(t *testing.T) {
	const provider, bucket, key = "local_minio", "fixture-bucket", "users/7/objects/merge +%.bin"
	const session, etag, checksum = "Fixture+A/= ", "Fixture-Object-2", "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	for _, fixture := range []struct {
		name, change, version string
		want                  error
	}{
		{name: "save opaque version", version: "Version+A/="},
		{name: "save explicit null", version: "null"},
		{name: "save unversioned result"},
		{name: "different observed size", want: domain.ErrUploadBodySizeMismatch},
		{name: "missing ETag", want: domain.ErrInvalidStoredObjectData},
		{name: "empty version", want: domain.ErrInvalidStoredObjectData},
		{name: "wrong status", change: "UPDATE upload_tasks SET status='UPLOADING' WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "stale version", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "changed owner", change: "UPDATE upload_tasks SET lease_owner='Merge-worker' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "changed session", change: "UPDATE upload_tasks SET storage_upload_id='Fixture+A/=' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "changed deadline", change: "UPDATE upload_tasks SET lease_until=lease_until+INTERVAL 1 SECOND WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "lease exactly expired", change: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "task exactly expired", change: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "changed declaration", change: "UPDATE upload_tasks SET expected_sha256=UNHEX(REPEAT('ff',32)) WHERE id=19", want: domain.ErrInvalidUploadTaskData},
		{name: "changed key", change: "UPDATE stored_objects SET object_key='other-key' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "changed bucket", change: "UPDATE stored_objects SET bucket='other-bucket' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "changed object status", change: "UPDATE stored_objects SET status='ORPHANED' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "existing object metadata", change: "UPDATE stored_objects SET storage_etag='other-etag' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "readback mismatch rolls back both rows", change: "CREATE TRIGGER fixture_merge_verify BEFORE UPDATE ON stored_objects FOR EACH ROW SET NEW.storage_version_id='drift'", want: domain.ErrInvalidStoredObjectData},
		{name: "wrong account", want: domain.ErrUploadTaskNotFound},
		{name: "canceled transaction", want: context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 在独占合成库准备已冻结的任务，走正式合并租约及恢复检查。
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
			execute("UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
			execute("UPDATE stored_objects SET provider=?,bucket=?,object_key=?,file_size=? WHERE id=23", provider, bucket, key, fileSize)
			execute(`UPDATE upload_tasks SET upload_mode='MULTIPART',status='COMPLETING',storage_upload_id=?,
				file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				lease_owner=NULL,lease_until=NULL,lease_version=4,next_attempt_at=UTC_TIMESTAMP(6),
				last_error_code='FIXTURE_RETRY',last_error_message='fixture retry',attempt_count=2,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`, session, fileSize, fileSize-1, fileSize)
			execute(`INSERT INTO upload_parts(task_id,part_number,etag,size,checksum_algorithm,checksum_value,observed_at) VALUES
				(19,1,'first',?,'SHA256',?,UTC_TIMESTAMP(6)),(19,2,'tail',1,'SHA256',?,UTC_TIMESTAMP(6))`, fileSize-1, checksum, checksum)
			candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 {
				t.Fatal("合成任务未进入候选", candidates, err)
			}
			claimed, err := store.ClaimMultipartUploadCompletionLease(ctx, candidates[0], "merge-worker")
			if err != nil {
				t.Fatal(err)
			}
			headResult := domain.UploadObjectResult{SizeBytes: fileSize, StorageETag: new(etag)}
			if fixture.version != "" {
				headResult.StorageVersionID = new(fixture.version)
			}
			targets := service.NewUploadPartSigningTargetService(provider, bucket, store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
			recovery := service.NewUploadMergeRecoveryService(targets.GetForMerge, func(_ context.Context, objectKey string) (*domain.UploadObjectResult, error) {
				if objectKey != key {
					t.Fatal("恢复检查未读取原对象键", objectKey)
				}
				return &headResult, nil
			})
			observation, err := recovery.Check(ctx, claimed)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 模拟观察结束后的变化，读回提交前的任务、对象和回执。
			if fixture.change != "" {
				execute(fixture.change)
			}
			writeContext := ctx
			switch fixture.name {
			case "wrong account":
				claimed.UserID = 8
			case "different observed size":
				observation.Object.SizeBytes--
			case "missing ETag":
				observation.Object.StorageETag = nil
			case "empty version":
				observation.Object.StorageVersionID = new("")
			case "canceled transaction":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				writeContext = canceled
			}
			read := func() (*domain.UploadTask, *domain.StoredObject, []models.UploadPart) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				var parts []models.UploadPart
				if err := store.database.WithContext(ctx).Where("task_id = ?", 19).Order("part_number ASC").Find(&parts).Error; err != nil {
					t.Fatal(err)
				}
				return task, object, parts
			}
			beforeTask, beforeObject, beforeParts := read()
			advance := func(callContext context.Context) (*domain.UploadTask, error) {
				return store.BeginMultipartUploadVerification(callContext, claimed, provider, bucket, observation.Target.ObjectKey, observation.Object)
			}
			result, err := advance(writeContext)
			if !errors.Is(err, fixture.want) {
				t.Fatalf("预期错误 %v，实际 %v", fixture.want, err)
			}
			if fixture.want == nil {
				// 3. 完整推进并释放租约，重复提交无法改写已确认版本。
				if result == nil {
					t.Fatal("提交成功未返回任务")
				}
				beforeTask.Status, beforeObject.Status = domain.UploadStatusVerifying, domain.StoredObjectStatusVerifying
				beforeTask.LeaseOwner, beforeTask.LeaseUntil, beforeTask.NextAttemptAt = nil, nil, nil
				beforeTask.LastErrorCode, beforeTask.LastErrorMessage, beforeTask.UpdatedAt = nil, nil, result.UpdatedAt
				beforeObject.StorageETag, beforeObject.StorageVersionID = observation.Object.StorageETag, observation.Object.StorageVersionID
				beforeObject.UpdatedAt = result.UpdatedAt
				if !reflect.DeepEqual(result, beforeTask) {
					t.Fatal("提交结果改变了预期之外的任务字段")
				}
				if repeated, err := advance(ctx); repeated != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
					t.Fatal("重复提交未被拒绝", repeated, err)
				}
			} else if result != nil {
				t.Fatal("失败时交付了任务")
			}
			afterTask, afterObject, afterParts := read()
			if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeObject, afterObject) || !reflect.DeepEqual(beforeParts, afterParts) {
				t.Fatal("事务提交或回滚后的任务、对象或回执不符")
			}
			var reserved, used uint64
			if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil || reserved != fileSize || used != 0 {
				t.Fatal("状态推进改变了配额", reserved, used, err)
			}
		})
	}
}
