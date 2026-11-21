package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFailMultipartUploadForMissingSessionIntegration(t *testing.T) {
	const provider, bucket, key = "local_minio", "fixture-bucket", "users/7/objects/lost-session.bin"
	const uploadID, checksum = "Opaque+A/= ", "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="
	const fileSize, otherReserved, used = domain.MaxSingleUploadSizeBytes + 1, 512, 1024
	for _, scenario := range []struct {
		name, change string
		want         error
	}{
		{name: "concurrent finalization releases once"},
		{name: "no missing observation", want: domain.ErrInvalidUploadTaskData},
		{name: "merge still uncertain", want: domain.ErrUploadTaskNotReady},
		{name: "stale lease", change: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "different lease deadline", change: "UPDATE upload_tasks SET lease_until=lease_until+INTERVAL 1 SECOND WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "expired lease", change: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "expired task", change: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "session changes", change: "UPDATE upload_tasks SET storage_upload_id='Opaque+A/=' WHERE id=19", want: domain.ErrUploadTaskBusy},
		{name: "another stage", change: "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", want: domain.ErrUploadTaskNotReady},
		{name: "object key changes", change: "UPDATE stored_objects SET object_key='other-key' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "object gains completion metadata", change: "UPDATE stored_objects SET storage_version_id='completed-version' WHERE id=23", want: domain.ErrInvalidStoredObjectData},
		{name: "active file still references object", change: "UPDATE file_entries SET object_id=23 WHERE id=31", want: domain.ErrStoredObjectInUse},
		{name: "account quota underflow", change: "UPDATE users SET reserved_storage=512 WHERE id=7", want: domain.ErrInvalidAccountData},
		{name: "quota readback rejects drift", change: "CREATE TRIGGER fixture_quota_drift BEFORE UPDATE ON users FOR EACH ROW SET NEW.used_storage=OLD.used_storage+1", want: domain.ErrInvalidAccountData},
		{name: "object update fails", change: "CREATE TRIGGER fixture_orphan_fault BEFORE UPDATE ON stored_objects FOR EACH ROW SIGNAL SQLSTATE 'HY000' SET MYSQL_ERRNO=1205, MESSAGE_TEXT='fixture lock wait'", want: domain.ErrUploadMergeWriteRetryable},
		{name: "canceled transaction", want: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 通过正式仓储冻结合成回执、领取租约，再生成缺失观察。
			ctx, pool, store := prepareMultipartCompletionDatabase(t)
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", used, fileSize+otherReserved)
			execute("UPDATE stored_objects SET provider=?,bucket=?,object_key=?,file_size=? WHERE id=23", provider, bucket, key, fileSize)
			execute(`UPDATE upload_tasks SET upload_mode='MULTIPART',status='UPLOADING',storage_upload_id=?,
				file_size=?,part_size=?,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				lease_owner=NULL,lease_until=NULL,lease_version=4,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`, uploadID, fileSize, fileSize-1, fileSize)
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			frozen := []domain.UploadPartReceipt{
				{PartNumber: 1, SizeBytes: fileSize - 1, ETag: "First", ChecksumSHA256: checksum},
				{PartNumber: 2, SizeBytes: 1, ETag: "Tail", ChecksumSHA256: checksum},
			}
			requestLease, err := store.ClaimUserMultipartCompletionLease(ctx, actor, readTask(), "verify-worker", service.ResolveMultipartUploadCompletionStatus)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitUserMultipartCompletion(ctx, actor, requestLease, frozen); err != nil {
				t.Fatal(err)
			}
			candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
				t.Fatal("未取得当前任务的合并候选", candidates, err)
			}
			lease, err := store.ClaimMultipartUploadCompletionLease(ctx, candidates[0], "merge-worker")
			if err != nil {
				t.Fatal(err)
			}
			targets := service.NewUploadPartSigningTargetService(provider, bucket, store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
			recovery := service.NewUploadMergeRecoveryService(targets.GetForMerge, func(context.Context, string) (*domain.UploadObjectResult, error) {
				return nil, domain.ErrUploadObjectMissing
			})
			receipts := service.NewUploadMergeReceiptService(targets.GetForMerge, store.ReadMultipartCompletionReceipts,
				func(context.Context, string, string) ([]domain.UploadPartReceipt, error) {
					return nil, domain.ErrUploadPartSessionMissing
				})
			checked, err := service.NewUploadMergePreflightService(recovery, receipts).CheckMissingSession(ctx, lease, domain.ErrUploadPartSessionMissing)
			if err != nil || checked == nil || checked.Missing == nil {
				t.Fatal("未取得原对象及会话的缺失观察", err)
			}

			// 2. 在事务前制造变化，保留可比较的任务、对象、配额及回执快照。
			if scenario.change != "" {
				execute(scenario.change)
			}
			type snapshot struct {
				task           *domain.UploadTask
				object         *domain.StoredObject
				receipts       []models.UploadPart
				used, reserved uint64
			}
			read := func() snapshot {
				t.Helper()
				staged, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				state := snapshot{task: readTask(), object: staged}
				if err := store.database.WithContext(ctx).Where("task_id = ?", 19).Order("part_number").Find(&state.receipts).Error; err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&state.used, &state.reserved); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := read()
			observation := checked.Missing
			mergeFinished := scenario.name != "merge still uncertain"
			writeContext := ctx
			switch scenario.name {
			case "no missing observation":
				observation = nil
			case "canceled transaction":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				writeContext = canceled
			}
			if scenario.want != nil {
				failed, err := store.FailMultipartUploadForMissingSession(writeContext, provider, bucket, observation, mergeFinished)
				if failed != nil || !errors.Is(err, scenario.want) || !reflect.DeepEqual(before, read()) {
					t.Fatal("收尾未拒绝或未完整回滚，预期", scenario.want, "实际", err)
				}
				if scenario.name == "object update fails" {
					var mysqlError *mysql.MySQLError
					if !errors.As(err, &mysqlError) || mysqlError.Number != 1205 {
						t.Fatal("对象写回的原故障未保留", err)
					}
				}
				return
			}

			// 3. 两次并发提交同一观察，仅一次改变任务、配额和对象状态。
			type outcome struct {
				task *domain.UploadTask
				err  error
			}
			finishes := make(chan outcome, 2)
			for range 2 {
				go func() {
					failed, err := store.FailMultipartUploadForMissingSession(ctx, provider, bucket, observation, true)
					finishes <- outcome{task: failed, err: err}
				}()
			}
			var committed *domain.UploadTask
			rejected := 0
			for range 2 {
				finished := <-finishes
				switch {
				case finished.err == nil && finished.task != nil:
					committed = finished.task
				case finished.task == nil && errors.Is(finished.err, domain.ErrUploadTaskNotReady):
					rejected++
				default:
					t.Error("并发收尾结果不符", finished)
				}
			}
			after := read()
			if committed == nil || rejected != 1 || !reflect.DeepEqual(committed, after.task) ||
				committed.Status != domain.UploadStatusFailed || committed.LastErrorCode == nil || *committed.LastErrorCode != "UPLOAD_SESSION_LOST" ||
				committed.CompletedAt == nil || committed.ReservedBytes != 0 || after.used != used || after.reserved != otherReserved ||
				!reflect.DeepEqual(before.receipts, after.receipts) {
				t.Fatal("失败终态、审计回执或只返还一次的配额不符", committed, after.reserved)
			}
			if after.object.Status != domain.StoredObjectStatusOrphaned || after.object.UnreferencedAt == nil || after.object.GCAfter == nil ||
				!after.object.GCAfter.Equal(after.object.UnreferencedAt.Add(7*24*time.Hour)) {
				t.Fatal("原暂存对象未进入常规延迟回收", after.object)
			}
			t.Logf("任务 %d 进入 %s，返还 %d 字节，其他预占 %d 字节，已用 %d 字节", lease.ID, committed.Status, fileSize, after.reserved, after.used)
		})
	}
}
