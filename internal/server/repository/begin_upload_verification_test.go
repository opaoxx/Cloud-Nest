package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBeginUserUploadVerificationResults(t *testing.T) {
	fault := errors.New("fixture verification transition failure")
	for _, mode := range []string{"ordinary", "no metadata", "record failure", "transition failure", "zero affected", "one affected", "three affected", "task snapshot failure", "task readback failure", "object readback failure", "wrong task state", "lease not released", "wrong version", "retry not cleared", "object wrong state", "object metadata changed", "object timestamp changed", "outer failure", "commit failure", "cancel during transition", "cancel during task readback", "cancel during object readback"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 使用同一脚本保存确认信息并推进两张表，注入阶段故障。
			task, taskValues, objectValues, _, now := uploadObjectReplacementFixture()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			task.AttemptCount = 4
			taskValues[20], taskValues[21] = now.Add(time.Hour), int64(4)
			taskValues[23], taskValues[24], objectValues[14] = "STORAGE_UNAVAILABLE", "旧的诊断", "STORAGE_RETRY"
			etag, version := "opaque-etag", "null"
			confirmation := domain.UploadObjectResult{SizeBytes: task.FileSize, StorageETag: &etag, StorageVersionID: &version}
			var want error
			switch mode {
			case "no metadata":
				confirmation.StorageETag, confirmation.StorageVersionID = nil, nil
			case "record failure", "transition failure", "task snapshot failure", "task readback failure", "object readback failure", "outer failure", "commit failure":
				want = fault
			case "zero affected":
				want = domain.ErrUploadTaskBusy
			case "one affected", "three affected", "wrong task state", "lease not released", "wrong version", "retry not cleared":
				want = domain.ErrInvalidUploadTaskData
			case "object wrong state", "object metadata changed", "object timestamp changed":
				want = domain.ErrInvalidStoredObjectData
			case "cancel during transition", "cancel during task readback", "cancel during object readback":
				want = context.Canceled
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			if mode == "commit failure" {
				script.commitError = fault
			}
			advanced, taskReads := false, 0
			script.uploadQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				taskReads++
				if !strings.HasSuffix(statement, "FOR UPDATE") || arguments[0].Value != actor.UserID || arguments[1].Value != task.ID {
					t.Error("任务查询缺少用户范围或当前读锁")
				}
				if (mode == "task snapshot failure" && taskReads == 2) || (advanced && mode == "task readback failure") {
					return nil, fault
				}
				values := append([]driver.Value(nil), taskValues...)
				if advanced {
					switch mode {
					case "wrong task state":
						values[14] = "UPLOADING"
					case "lease not released":
						values[17], values[18] = *task.LeaseOwner, *task.LeaseUntil
					case "wrong version":
						values[19] = "4"
					case "retry not cleared":
						values[20] = now.Add(time.Hour)
					case "cancel during task readback":
						cancel()
					}
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.objectQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(statement, "FOR UPDATE") || arguments[0].Value != actor.UserID || arguments[1].Value != task.StagedObjectID {
					t.Error("对象查询缺少用户范围或当前读锁")
				}
				if advanced && mode == "object readback failure" {
					return nil, fault
				}
				values := append([]driver.Value(nil), objectValues...)
				if advanced {
					switch mode {
					case "object wrong state":
						values[8] = "STAGING"
					case "object metadata changed":
						values[10] = "other-etag"
					case "object timestamp changed":
						values[16] = now
					case "cancel during object readback":
						cancel()
					}
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			script.recordUploadObject = func(_ context.Context, _ string, arguments []driver.NamedValue) (driver.Result, error) {
				if mode == "record failure" {
					return nil, fault
				}
				objectValues[10], objectValues[5], objectValues[16] = arguments[0].Value, arguments[1].Value, now.Add(time.Microsecond)
				return driver.RowsAffected(1), nil
			}
			script.beginUploadVerification = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				// 2. 双表条件更新同时设置状态，释放接收租约并清理旧调度信息。
				for _, fragment := range []string{"upload_task.status = 'VERIFYING'", "stored_object.status = 'VERIFYING'", "upload_task.lease_owner = NULL", "upload_task.lease_until = NULL", "upload_task.next_attempt_at = NULL", "upload_task.last_error_code = NULL", "stored_object.last_error_code = NULL", "upload_task.lease_version = ?", "upload_task.lease_until > UTC_TIMESTAMP(6)", "upload_task.expires_at > UTC_TIMESTAMP(6)", "stored_object.status = 'STAGING'"} {
					if !strings.Contains(statement, fragment) {
						t.Error("双表状态更新缺少约束", fragment)
					}
				}
				actual := make([]any, len(arguments))
				for index, argument := range arguments {
					actual[index] = argument.Value
				}
				if !reflect.DeepEqual(actual, []any{actor.UserID, task.ID, task.StagedObjectID, *task.LeaseOwner, task.LeaseVersion}) {
					t.Error("状态更新范围参数错误", actual)
				}
				switch mode {
				case "transition failure":
					return nil, fault
				case "zero affected":
					return driver.RowsAffected(0), nil
				case "one affected":
					return driver.RowsAffected(1), nil
				case "three affected":
					return driver.RowsAffected(3), nil
				}
				advanced = true
				taskValues[14], taskValues[17], taskValues[18], taskValues[20] = "VERIFYING", nil, nil, nil
				taskValues[23], taskValues[24], taskValues[26] = nil, nil, now.Add(2*time.Microsecond)
				objectValues[8], objectValues[14], objectValues[16] = "VERIFYING", nil, now.Add(2*time.Microsecond)
				if mode == "cancel during transition" {
					cancel()
				}
				return driver.RowsAffected(2), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				result, err := txStore.BeginUserUploadVerification(txContext, task.ID, task.StagedObjectID, task.LeaseVersion, *task.LeaseOwner, confirmation)
				if err != nil {
					if result != nil {
						t.Error("进入校验失败仍交付任务")
					}
					return err
				}
				expected := task
				expected.Status, expected.LeaseOwner, expected.LeaseUntil = domain.UploadStatusVerifying, nil, nil
				expected.UpdatedAt = now.Add(2 * time.Microsecond)
				if !reflect.DeepEqual(result, &expected) {
					t.Error("校验任务的完整结果不符")
				}
				if mode == "outer failure" {
					return fault
				}
				return nil
			})
			if !errors.Is(err, want) {
				t.Fatal("校验阶段事务结果错误", err, want)
			}
			if errors.Is(err, context.Canceled) {
				select {
				case <-script.rolledBack:
				case <-time.After(5 * time.Second):
					t.Fatal("取消后的回滚未完成")
				}
			}
			if want != nil && mode != "commit failure" && !strings.Contains(script.sequence(), "rollback") {
				t.Fatal("状态推进失败未回滚", script.sequence())
			}
		})
	}
}

func TestBeginUserUploadVerificationInputs(t *testing.T) {
	var missing *Store
	if result, err := missing.BeginUserUploadVerification(context.Background(), 19, 23, 3, "worker", domain.UploadObjectResult{}); err == nil || result != nil {
		t.Fatal("空仓储未拒绝")
	}
	if result, err := missing.BeginUserUploadVerification(nil, 19, 23, 3, "worker", domain.UploadObjectResult{}); err == nil || result != nil {
		t.Fatal("空上下文未拒绝")
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	script := &loginTransactionScript{}
	store := revokeSessionTestStore(t, actor, script)
	store.loginUserID, store.authenticatedActor = actor.UserID, &actor
	if result, err := store.BeginUserUploadVerification(context.Background(), 19, 23, 3, "worker", domain.UploadObjectResult{}); err == nil || result != nil {
		t.Fatal("事务外推进被接受")
	}
}
