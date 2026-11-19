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

func TestWithUploadVerificationTransactionResults(t *testing.T) {
	fault := errors.New("fixture verification transaction failure")
	for _, fixture := range []struct {
		name string
		want error
		call bool
	}{
		{"valid", nil, true},
		{"renewed lease and maximum version", nil, true},
		{"no expected checksum", nil, true},
		{"digest size mismatch", domain.ErrUploadBodySizeMismatch, false},
		{"digest hash mismatch", domain.ErrUploadChecksumMismatch, false},
		{"scope mismatch", domain.ErrInvalidStoredObjectData, false},
		{"missing lease snapshot", domain.ErrInvalidUploadTaskData, false},
		{"disabled user", domain.ErrAccountDisabled, false},
		{"changed stage", domain.ErrUploadTaskNotReady, false},
		{"changed task object", domain.ErrUploadTaskBusy, false},
		{"changed owner", domain.ErrUploadTaskBusy, false},
		{"changed version", domain.ErrUploadTaskBusy, false},
		{"changed expected checksum", domain.ErrInvalidUploadTaskData, false},
		{"changed reservation", domain.ErrInvalidUploadTaskData, false},
		{"changed key", domain.ErrInvalidStoredObjectData, false},
		{"changed storage version", domain.ErrInvalidStoredObjectData, false},
		{"changed ETag", domain.ErrInvalidStoredObjectData, false},
		{"wrong object phase", domain.ErrInvalidStoredObjectData, false},
		{"task expires now", domain.ErrUploadTaskNotReady, false},
		{"lease expires now", domain.ErrUploadTaskBusy, false},
		{"cancel during clock", context.Canceled, false},
		{"callback failure", fault, true},
		{"commit failure", fault, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task, taskValues, _, _, now := uploadObjectReplacementFixture()
			task.Status, taskValues[14] = domain.UploadStatusVerifying, "VERIFYING"
			object, objectValues := stagedObjectCreateFixture(domain.CreateStagedObjectInput{
				Provider: "local_minio", Bucket: "fixture-bucket", ObjectKey: "verification-object", SizeBytes: task.FileSize,
			})
			object.Status, objectValues[8] = domain.StoredObjectStatusVerifying, "VERIFYING"
			object.StorageVersionID, objectValues[5] = new("null"), "null"
			digest := domain.UploadContentDigest{SizeBytes: task.FileSize, SHA256: *task.ExpectedSHA256}
			script := &loginTransactionScript{status: "ACTIVE"}
			switch fixture.name {
			case "renewed lease and maximum version":
				task.LeaseVersion, taskValues[19], taskValues[18] = ^uint64(0), "18446744073709551615", now.Add(2*time.Minute)
			case "no expected checksum":
				task.ExpectedSHA256, taskValues[10] = nil, nil
			case "digest size mismatch":
				digest.SizeBytes++
			case "digest hash mismatch":
				digest.SHA256[0] ^= 1
			case "scope mismatch":
				object.OwnerUserID++
			case "missing lease snapshot":
				task.LeaseOwner = nil
			case "disabled user":
				script.status = "DISABLED"
			case "changed stage":
				taskValues[14] = "CANCELLING"
			case "changed task object":
				taskValues[3] = "24"
			case "changed owner":
				taskValues[17] = "another-worker"
			case "changed version":
				taskValues[19] = "4"
			case "changed expected checksum":
				taskValues[10] = nil
			case "changed reservation":
				taskValues[16] = "0"
			case "changed key":
				objectValues[4] = "new-key"
			case "changed storage version":
				objectValues[5] = nil
			case "changed ETag":
				objectValues[10] = ""
			case "wrong object phase":
				objectValues[8] = "STAGING"
			case "task expires now":
				taskValues[22] = now
			case "lease expires now":
				taskValues[18] = now
			case "commit failure":
				script.commitError = fault
			}
			store := loginTransactionTestStore(t, script)
			script.uploadQuery = func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(query, "FOR UPDATE") || args[0].Value != task.UserID || args[1].Value != task.ID {
					t.Error("任务读缺少用户范围或锁")
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: taskValues}}, nil
			}
			script.objectQuery = func(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(query, "FOR UPDATE") || args[0].Value != task.UserID || args[1].Value != object.ID {
					t.Error("对象读缺少用户范围或锁")
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: objectValues}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if fixture.name == "cancel during clock" {
					cancel()
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			calls := 0
			err := store.WithUploadVerificationTransaction(ctx, &task, &object, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
				calls++
				if txStore == store || txStore.loginUserID != task.UserID || txContext == ctx || txContext.Err() != nil ||
					current.ID != task.ID || current.LeaseVersion != task.LeaseVersion || !reflect.DeepEqual(staged, &object) {
					t.Error("回调未取得当前锁定数据")
				}
				if fixture.name == "renewed lease and maximum version" && !current.LeaseUntil.After(*task.LeaseUntil) {
					t.Error("续租后仍使用原期限")
				}
				script.record("action")
				if fixture.name == "callback failure" {
					return fault
				}
				return nil
			})
			if !errors.Is(err, fixture.want) || calls > 1 || (calls == 1) != fixture.call {
				t.Fatal("复核错误或回调次数不符", err, calls, script.sequence())
			}
			if err == nil && script.sequence() != "begin,lock,upload,object,lease_clock,action,commit" {
				t.Fatal("加锁、回调和提交顺序错误", script.sequence())
			}
			if fixture.want == context.Canceled {
				select {
				case <-script.rolledBack:
				case <-time.After(5 * time.Second):
					t.Fatal("取消后未回滚")
				}
			}
		})
	}
}
