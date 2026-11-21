package repository

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestReopenMultipartUploadForRepairIntegration(t *testing.T) {
	const provider, bucket, key = "local_minio", "fixture-bucket", "users/7/objects/repair +%.bin"
	const session, checksum = "Opaque+A/= ", "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	for _, fixture := range []struct {
		name, change string
		want         error
		mysqlCode    uint16
	}{
		{name: "reopen and freeze new receipts"},
		{name: "missing evidence", want: domain.ErrInvalidUploadTaskData},
		{name: "empty mismatch", want: domain.ErrInvalidUploadTaskData},
		{name: "duplicate mismatch numbers", want: domain.ErrInvalidUploadTaskData},
		{name: "out of plan mismatch", want: domain.ErrInvalidUploadTaskData},
		{name: "wrong account", want: domain.ErrUploadTaskNotFound},
		{name: "task status changed", change: "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "lease replaced", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "lease deadline changed", change: "UPDATE upload_tasks SET lease_until=lease_until+INTERVAL 1 SECOND WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "lease expires exactly", change: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "task expires exactly", change: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "session changes", change: "UPDATE upload_tasks SET storage_upload_id='Opaque+A/=' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "object key changes", change: "UPDATE stored_objects SET object_key='other-key' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "object has completion metadata", change: "UPDATE stored_objects SET storage_etag='done' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "freeze loses a row", change: "DELETE FROM upload_parts WHERE task_id=19 AND part_number=2", want: domain.ErrInvalidUploadTaskData},
		{name: "delete failure rolls back state", change: "CREATE TRIGGER fixture_repair_delete BEFORE DELETE ON upload_parts FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture reject delete'", mysqlCode: 1644},
		{name: "readback failure restores old rows", change: "CREATE TRIGGER fixture_repair_drift BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.storage_upload_id='drift'", want: domain.ErrInvalidUploadTaskData},
		{name: "canceled context", want: context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 在独占库冻结两个任务的合成回执，为任务 19 生成修复证据。
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
			execute("UPDATE users SET reserved_storage=? WHERE id IN (7,8)", fileSize)
			execute("UPDATE stored_objects SET provider=?,bucket=?,object_key=?,file_size=? WHERE id=23", provider, bucket, key, fileSize)
			execute("UPDATE stored_objects SET file_size=? WHERE id=24", fileSize)
			execute(`UPDATE upload_tasks SET upload_mode='MULTIPART',status='UPLOADING',storage_upload_id=?,
				file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				lease_owner=NULL,lease_until=NULL,lease_version=4,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`, session, fileSize, fileSize-1, fileSize)
			execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
				expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
				VALUES(20,8,12,24,14,'other.bin','MULTIPART','other-session',UNHEX(REPEAT('cd',32)),?,?,2,'COMPLETING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`, fileSize, fileSize-1, fileSize)
			execute(`INSERT INTO upload_parts(task_id,part_number,etag,size,checksum_algorithm,checksum_value,observed_at) VALUES
				(20,1,'other-first',?,'SHA256',?,UTC_TIMESTAMP(6)),(20,2,'other-tail',1,'SHA256',?,UTC_TIMESTAMP(6))`, fileSize-1, checksum, checksum)
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			frozen := []domain.UploadPartReceipt{
				{PartNumber: 1, SizeBytes: fileSize - 1, ETag: "OldFirst", ChecksumSHA256: checksum},
				{PartNumber: 2, SizeBytes: 1, ETag: "Tail", ChecksumSHA256: checksum},
			}
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			freeze := func(parts []domain.UploadPartReceipt) {
				t.Helper()
				lease, err := store.ClaimUserMultipartCompletionLease(ctx, actor, readTask(), "verify-worker", service.ResolveMultipartUploadCompletionStatus)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.CommitUserMultipartCompletion(ctx, actor, lease, parts); err != nil {
					t.Fatal(err)
				}
			}
			freeze(frozen)
			candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
				t.Fatal("未取得任务 19 的候选", candidates, err)
			}
			lease, err := store.ClaimMultipartUploadCompletionLease(ctx, candidates[0], "merge-worker")
			if err != nil {
				t.Fatal(err)
			}
			observed := slices.Clone(frozen)
			observed[0].ETag = "NewFirst"
			targets := service.NewUploadPartSigningTargetService(provider, bucket, store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
			recovery := service.NewUploadMergeRecoveryService(targets.GetForMerge, func(context.Context, string) (*domain.UploadObjectResult, error) {
				return nil, domain.ErrUploadObjectMissing
			})
			receipts := service.NewUploadMergeReceiptService(targets.GetForMerge, store.ReadMultipartCompletionReceipts,
				func(context.Context, string, string) ([]domain.UploadPartReceipt, error) { return observed, nil })
			evidence, err := service.NewUploadMergePreflightService(recovery, receipts).CheckRepair(ctx, lease, domain.ErrMultipartUploadPartRejected)
			if err != nil || evidence == nil || !slices.Equal(evidence.PartNumbers, []uint32{1}) {
				t.Fatal("未取得任务的只读修复证据", evidence, err)
			}

			// 2. 模拟证据产生后的变化，再比较写回前后的记录。
			if fixture.change != "" {
				execute(fixture.change)
			}
			writeContext := ctx
			switch fixture.name {
			case "missing evidence":
				evidence = nil
			case "empty mismatch":
				evidence.PartNumbers = nil
			case "duplicate mismatch numbers":
				evidence.PartNumbers = []uint32{1, 1}
			case "out of plan mismatch":
				evidence.PartNumbers = []uint32{3}
			case "wrong account":
				evidence.Task.UserID = 8
			case "canceled context":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				writeContext = canceled
			}
			type snapshot struct {
				task           *domain.UploadTask
				object         *domain.StoredObject
				parts          []models.UploadPart
				reserved, used uint64
			}
			read := func() snapshot {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				state := snapshot{task: readTask(), object: object}
				if err := store.database.WithContext(ctx).Where("task_id IN ?", []uint64{19, 20}).Order("task_id ASC, part_number ASC").Find(&state.parts).Error; err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&state.reserved, &state.used); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := read()
			result, err := store.ReopenMultipartUploadForRepair(writeContext, provider, bucket, evidence)
			if fixture.mysqlCode != 0 {
				var mysqlError *mysql.MySQLError
				if !errors.As(err, &mysqlError) || mysqlError.Number != fixture.mysqlCode {
					t.Fatal("预期隔离触发器拒绝删除", err)
				}
			} else if !errors.Is(err, fixture.want) {
				t.Fatalf("预期错误 %v，实际 %v", fixture.want, err)
			}
			if err != nil {
				if result != nil || !reflect.DeepEqual(before, read()) {
					t.Fatal("失败后任务、对象、回执或配额改变")
				}
				return
			}

			// 3. 只清除任务 19 的旧回执。新一轮冻结回执后，旧证据仍无权写回。
			if result == nil {
				t.Fatal("恢复成功未返回任务")
			}
			before.task.Status, before.task.LeaseOwner, before.task.LeaseUntil = domain.UploadStatusUploading, nil, nil
			before.task.NextAttemptAt, before.task.LastErrorCode, before.task.LastErrorMessage = nil, nil, nil
			before.task.UpdatedAt = result.UpdatedAt
			before.parts = before.parts[2:]
			if !reflect.DeepEqual(before.task, result) || !reflect.DeepEqual(before, read()) {
				t.Fatal("恢复提交改变了预期之外的字段或其他任务回执")
			}
			if repeated, err := store.ReopenMultipartUploadForRepair(ctx, provider, bucket, evidence); repeated != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("重复恢复未被拒绝", repeated, err)
			}
			freeze(observed)
			newRound := read()
			if newRound.task.Status != domain.UploadStatusCompleting || len(newRound.parts) != 4 || newRound.parts[0].ETag != "NewFirst" {
				t.Fatal("原会话未成功提交替换后的回执")
			}
			if repeated, err := store.ReopenMultipartUploadForRepair(ctx, provider, bucket, evidence); repeated != nil || !errors.Is(err, domain.ErrUploadTaskBusy) ||
				!reflect.DeepEqual(newRound, read()) {
				t.Fatal("旧证据影响了新一轮完成快照", repeated, err)
			}
		})
	}
}
