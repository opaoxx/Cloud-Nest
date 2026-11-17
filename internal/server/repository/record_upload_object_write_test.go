package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRecordUserUploadObjectWriteResults(t *testing.T) {
	fault := errors.New("fixture metadata failure")
	for _, mode := range []string{"ordinary", "no metadata", "empty metadata", "fill version", "preserve known", "repeat", "conflict", "wrong object", "wrong owner", "wrong version", "wrong stage", "missing task", "size mismatch", "object stage", "task expired", "lease expired", "clock failure", "missing clock", "invalid clock", "write failure", "zero affected", "multiple affected", "readback failure", "wrong readback", "outer failure", "commit failure", "copy input", "cancel during object", "cancel during clock", "cancel during write", "cancel during readback"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 复用已领取租约的任务与初始对象夹具。
			task, taskValues, objectValues, _, now := uploadObjectReplacementFixture()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			etag, version := "opaque-etag", "null"
			confirmation := domain.UploadObjectResult{SizeBytes: task.FileSize, StorageETag: &etag, StorageVersionID: &version}
			objectID, leaseVersion, owner := task.StagedObjectID, task.LeaseVersion, *task.LeaseOwner
			wantETag, wantVersion := copyUploadTaskValue(&etag), copyUploadTaskValue(&version)
			var want error
			write := true
			switch mode {
			case "no metadata":
				confirmation.StorageETag, confirmation.StorageVersionID = nil, nil
				wantETag, wantVersion, write = nil, nil, false
			case "empty metadata":
				etag, version = "", ""
				wantETag, wantVersion = copyUploadTaskValue(&etag), copyUploadTaskValue(&version)
			case "fill version":
				objectValues[10], confirmation.StorageETag = etag, nil
			case "preserve known", "repeat":
				objectValues[10], objectValues[5], write = etag, version, false
				if mode == "preserve known" {
					confirmation.StorageETag, confirmation.StorageVersionID = nil, nil
				}
			case "conflict":
				objectValues[10], want = "existing-etag", domain.ErrInvalidStoredObjectData
			case "wrong object":
				objectID, want = 25, domain.ErrUploadTaskBusy
			case "wrong owner":
				owner, want = "old-worker", domain.ErrUploadTaskBusy
			case "wrong version":
				leaseVersion, want = 2, domain.ErrUploadTaskBusy
			case "wrong stage":
				taskValues[14], want = "VERIFYING", domain.ErrUploadTaskNotReady
			case "missing task":
				taskValues, want = nil, domain.ErrUploadTaskNotFound
			case "size mismatch":
				confirmation.SizeBytes, want = task.FileSize-1, domain.ErrUploadBodySizeMismatch
			case "object stage":
				objectValues[8], want = "VERIFYING", domain.ErrInvalidStoredObjectData
			case "task expired":
				taskValues[22], want = now, domain.ErrUploadTaskNotReady
			case "lease expired":
				taskValues[18], want = now, domain.ErrUploadTaskBusy
			case "clock failure", "write failure", "readback failure", "outer failure", "commit failure":
				want = fault
			case "missing clock":
				want = sql.ErrNoRows
			case "invalid clock":
				want = domain.ErrInvalidUploadTaskData
			case "zero affected":
				want = domain.ErrUploadTaskBusy
			case "multiple affected", "wrong readback":
				want = domain.ErrInvalidStoredObjectData
			case "cancel during object", "cancel during clock", "cancel during write", "cancel during readback":
				want = context.Canceled
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			if mode == "commit failure" {
				script.commitError = fault
			}
			script.uploadQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(statement, "FOR UPDATE") || arguments[0].Value != actor.UserID || arguments[1].Value != task.ID {
					t.Error("任务未按用户和任务范围当前读")
				}
				if mode == "copy input" {
					etag, version = "changed-after-entry", "changed-after-entry"
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: taskValues}}, nil
			}
			reads, writes := 0, 0
			script.objectQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if !strings.HasSuffix(statement, "FOR UPDATE") || arguments[0].Value != actor.UserID || arguments[1].Value != task.StagedObjectID {
					t.Error("对象未按归属当前读")
				}
				values := append([]driver.Value(nil), objectValues...)
				if reads == 2 {
					if mode == "readback failure" {
						return nil, fault
					}
					values[5], values[10], values[16] = nil, nil, now.Add(time.Microsecond)
					if wantETag != nil {
						values[10] = *wantETag
					}
					if wantVersion != nil {
						values[5] = *wantVersion
					}
					if mode == "wrong readback" {
						values[10] = "wrong-saved-etag"
					}
				}
				if (reads == 1 && mode == "cancel during object") || (reads == 2 && mode == "cancel during readback") {
					cancel()
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if mode == "clock failure" {
					return nil, fault
				}
				if mode == "missing clock" {
					return &uploadLeaseClockRows{&idempotencyRecordRows{}}, nil
				}
				stamp := now
				if mode == "invalid clock" {
					stamp = now.Add(time.Nanosecond)
				}
				if mode == "cancel during clock" {
					cancel()
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{stamp}}}, nil
			}
			script.recordUploadObject = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				// 2. 条件更新必须绑定当前任务、对象、持有者和完整租约版本。
				writes++
				for _, fragment := range []string{"stored_object.owner_user_id = ?", "stored_object.id = ?", "upload_task.id = ?", "upload_task.lease_owner = ?", "upload_task.lease_version = ?", "upload_task.lease_until > UTC_TIMESTAMP(6)", "upload_task.expires_at > UTC_TIMESTAMP(6)", "stored_object.status = 'STAGING'", "upload_task.status = 'UPLOADING'"} {
					if !strings.Contains(statement, fragment) {
						t.Error("条件写入缺少约束", fragment)
					}
				}
				expected := []any{nil, nil, actor.UserID, task.StagedObjectID, task.ID, *task.LeaseOwner, task.LeaseVersion}
				if wantETag != nil {
					expected[0] = *wantETag
				}
				if wantVersion != nil {
					expected[1] = *wantVersion
				}
				actual := make([]any, len(arguments))
				for index, argument := range arguments {
					actual[index] = argument.Value
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Error("条件写入参数或输入副本错误", actual, expected)
				}
				switch mode {
				case "write failure":
					return nil, fault
				case "zero affected":
					return driver.RowsAffected(0), nil
				case "multiple affected":
					return driver.RowsAffected(2), nil
				case "cancel during write":
					cancel()
				}
				return driver.RowsAffected(1), nil
			}
			// 3. 结果先留在事务内，后续失败和提交失败由外层处理。
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				result, err := txStore.RecordUserUploadObjectWrite(txContext, task.ID, objectID, leaseVersion, owner, confirmation)
				if err != nil {
					if result != nil {
						t.Error("失败仍返回对象")
					}
					return err
				}
				if result == nil || result.ID != task.StagedObjectID || result.FileSize != task.FileSize || result.Status != domain.StoredObjectStatusStaging || !sameUploadStorageText(result.StorageETag, wantETag) || !sameUploadStorageText(result.StorageVersionID, wantVersion) {
					t.Error("写入确认结果错误")
				}
				if result != nil && ((result.StorageETag != nil && result.StorageETag == confirmation.StorageETag) || (result.StorageVersionID != nil && result.StorageVersionID == confirmation.StorageVersionID)) {
					t.Error("返回结果复用了调用方元数据指针")
				}
				if !write && writes != 0 {
					t.Error("重复确认再次更新记录")
				}
				if mode == "outer failure" {
					return fault
				}
				return nil
			})
			if !errors.Is(err, want) {
				t.Fatal("事务结果错误", err, want)
			}
			if errors.Is(err, context.Canceled) {
				select {
				case <-script.rolledBack:
				case <-time.After(5 * time.Second):
					t.Fatal("取消后的事务回滚未完成")
				}
			}
			if want != nil && mode != "commit failure" && !strings.Contains(script.sequence(), "rollback") {
				t.Fatal("失败未回滚事务", script.sequence())
			}
		})
	}
}

func TestRecordUserUploadObjectWriteInputs(t *testing.T) {
	var missing *Store
	if result, err := missing.RecordUserUploadObjectWrite(context.Background(), 19, 23, 3, "worker", domain.UploadObjectResult{}); err == nil || result != nil {
		t.Fatal("空仓储未拒绝")
	}
	for _, mode := range []string{"nil context", "zero task", "zero object", "zero version", "empty owner", "long owner", "Unicode owner", "long metadata", "Unicode metadata", "oversized", "wrong actor", "outside transaction"} {
		t.Run(mode, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			if mode == "outside transaction" {
				store.loginUserID, store.authenticatedActor = actor.UserID, &actor
				if result, err := store.RecordUserUploadObjectWrite(context.Background(), 19, 23, 3, "worker", domain.UploadObjectResult{}); err == nil || result != nil {
					t.Fatal("事务外调用被接受")
				}
				return
			}
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				taskID, objectID, version, owner := uint64(19), uint64(23), uint64(3), "worker"
				confirmation := domain.UploadObjectResult{SizeBytes: 2048}
				var want error
				switch mode {
				case "nil context":
					txContext = nil
				case "zero task":
					taskID = 0
				case "zero object":
					objectID = 0
				case "zero version":
					version, want = 0, domain.ErrInvalidUploadTaskData
				case "empty owner":
					owner, want = "", domain.ErrInvalidUploadTaskData
				case "long owner":
					owner, want = strings.Repeat("x", 65), domain.ErrInvalidUploadTaskData
				case "Unicode owner":
					owner, want = "持有者", domain.ErrInvalidUploadTaskData
				case "long metadata", "Unicode metadata":
					value := strings.Repeat("x", 256)
					if mode == "Unicode metadata" {
						value = "元数据"
					}
					confirmation.StorageETag, want = &value, domain.ErrInvalidStoredObjectData
				case "oversized":
					confirmation.SizeBytes, want = domain.MaxSingleUploadSizeBytes+1, domain.ErrUploadFileTooLarge
				case "wrong actor":
					wrong := actor
					wrong.UserID = 8
					txStore.authenticatedActor, want = &wrong, domain.ErrInvalidSessionData
				}
				result, err := txStore.RecordUserUploadObjectWrite(txContext, taskID, objectID, version, owner, confirmation)
				if err == nil || result != nil || (want != nil && !errors.Is(err, want)) || strings.Contains(script.sequence(), "upload") || strings.Contains(script.sequence(), "object") {
					t.Error("输入错误未在读取任务前结束", err)
				}
				return err
			})
			if err == nil {
				t.Fatal("无效输入事务未失败")
			}
		})
	}
}
