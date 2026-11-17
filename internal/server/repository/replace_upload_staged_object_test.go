package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadObjectReplacementFixture 组合已领取租约的任务与两个独立暂存对象。
func uploadObjectReplacementFixture() (domain.UploadTask, []driver.Value, []driver.Value, []driver.Value, time.Time) {
	digest := sha256.Sum256([]byte("replacement fixture"))
	expiry := time.Date(2026, 11, 18, 8, 0, 0, 123456000, time.UTC)
	task, taskValues := uploadTaskCreateFixture(domain.CreateUploadInput{ParentID: 13, Name: "学习.bin", SizeBytes: 2048, ExpectedSHA256: &digest}, 23, expiry)
	now := task.CreatedAt.Add(time.Hour)
	owner, until := "current-worker", now.Add(time.Minute)
	task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, 3
	taskValues[17], taskValues[18], taskValues[19] = owner, until, "3"
	_, oldValues := stagedObjectCreateFixture(domain.CreateStagedObjectInput{Provider: "MINIO", Bucket: "fixture-bucket", ObjectKey: "old-attempt", SizeBytes: 2048})
	_, newValues := stagedObjectCreateFixture(domain.CreateStagedObjectInput{Provider: "MINIO", Bucket: "fixture-bucket", ObjectKey: "new-attempt", SizeBytes: 2048})
	newValues[0] = "25"
	return task, taskValues, oldValues, newValues, now
}

func TestReplaceUserUploadStagedObjectResults(t *testing.T) {
	fault := errors.New("private replacement failure")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private staged reference duplicate"}
	for _, mode := range []string{"ordinary", "zero bytes", "single limit", "maximum version", "maximum identifiers", "old storage metadata", "missing task", "task query failure", "wrong stage", "oversize", "partial reservation", "wrong old object", "wrong owner", "wrong version", "missing lease", "task expired", "lease expired", "missing old", "missing new", "old query failure", "new query failure", "wrong old state", "wrong old size", "old GC data", "wrong new owner", "wrong new state", "wrong new size", "new storage version", "new digest", "new ETag", "new verified time", "new GC data", "new error code", "same location", "clock failure", "missing clock", "invalid clock", "duplicate reference", "wrapped duplicate", "write failure", "zero affected", "multiple affected", "missing readback", "readback failure", "wrong object readback", "wrong lease readback", "wrong version readback", "old update time", "outer failure", "commit failure"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备独立任务/对象事实，按场景变更预期值或持久记录。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			want, taskValues, oldValues, newValues, now := uploadObjectReplacementFixture()
			oldID, newID, version, owner := uint64(23), uint64(25), uint64(3), "current-worker"
			writeTime := now.Add(time.Microsecond)
			var wantError error
			script := &loginTransactionScript{}
			switch mode {
			case "ordinary", "old storage metadata":
				if mode == "old storage metadata" {
					oldValues[5], oldValues[10] = "old-version", "old-etag"
				}
			case "zero bytes", "single limit":
				size := uint64(0)
				if mode == "single limit" {
					size = domain.MaxSingleUploadSizeBytes
				}
				want.FileSize, want.ReservedBytes = size, size
				taskValues[11], taskValues[16], oldValues[6], newValues[6] = strconv.FormatUint(size, 10), strconv.FormatUint(size, 10), strconv.FormatUint(size, 10), strconv.FormatUint(size, 10)
			case "maximum version":
				version, want.LeaseVersion, taskValues[19] = ^uint64(0), ^uint64(0), "18446744073709551615"
			case "maximum identifiers":
				want.ID, want.StagedObjectID, oldID, newID = ^uint64(0), ^uint64(0)-1, ^uint64(0)-1, ^uint64(0)
				taskValues[0], taskValues[3], oldValues[0], newValues[0] = "18446744073709551615", "18446744073709551614", "18446744073709551614", "18446744073709551615"
			case "missing task":
				wantError = domain.ErrUploadTaskNotFound
			case "task query failure", "old query failure", "new query failure", "clock failure", "write failure", "readback failure", "outer failure", "commit failure":
				wantError = fault
			case "wrong stage":
				taskValues[14], wantError = "VERIFYING", domain.ErrUploadTaskNotReady
			case "oversize":
				taskValues[11], taskValues[16], wantError = "16777217", "16777217", domain.ErrUploadFileTooLarge
			case "partial reservation":
				taskValues[16], wantError = "2047", domain.ErrInvalidUploadTaskData
			case "wrong old object", "wrong owner", "wrong version", "missing lease", "lease expired", "zero affected":
				wantError = domain.ErrUploadTaskBusy
				if mode == "missing lease" {
					taskValues[17], taskValues[18] = nil, nil
				}
				if mode == "lease expired" {
					taskValues[18] = now
				}
			case "task expired":
				taskValues[22], wantError = now, domain.ErrUploadTaskNotReady
			case "missing old", "missing new":
				wantError = domain.ErrStoredObjectNotFound
			case "wrong old state":
				oldValues[8], wantError = "ORPHANED", domain.ErrInvalidStoredObjectData
			case "wrong old size":
				oldValues[6], wantError = "1", domain.ErrInvalidStoredObjectData
			case "old GC data":
				oldValues[12], oldValues[13], wantError = now, now.Add(7*24*time.Hour), domain.ErrInvalidStoredObjectData
			case "wrong new owner":
				newValues[1], wantError = "8", domain.ErrInvalidStoredObjectData
			case "wrong new state":
				newValues[8], wantError = "VERIFYING", domain.ErrInvalidStoredObjectData
			case "wrong new size":
				newValues[6], wantError = "1", domain.ErrInvalidStoredObjectData
			case "new storage version":
				newValues[5], wantError = "version", domain.ErrInvalidStoredObjectData
			case "new digest":
				newValues[7], wantError = make([]byte, sha256.Size), domain.ErrInvalidStoredObjectData
			case "new ETag":
				newValues[10], wantError = "etag", domain.ErrInvalidStoredObjectData
			case "new verified time":
				newValues[11], wantError = now, domain.ErrInvalidStoredObjectData
			case "new GC data":
				newValues[12], newValues[13], wantError = now, now.Add(7*24*time.Hour), domain.ErrInvalidStoredObjectData
			case "new error code":
				newValues[14], wantError = "STORAGE_RETRY", domain.ErrInvalidStoredObjectData
			case "same location":
				newValues[4], wantError = oldValues[4], domain.ErrInvalidStoredObjectData
			case "missing clock":
				wantError = sql.ErrNoRows
			case "invalid clock", "multiple affected", "missing readback", "wrong object readback", "wrong lease readback", "wrong version readback", "old update time":
				wantError = domain.ErrInvalidUploadTaskData
			case "duplicate reference", "wrapped duplicate":
				wantError = domain.ErrUploadTaskStagedObjectConflict
			}
			store := revokeSessionTestStore(t, actor, script)
			if mode == "commit failure" {
				script.commitError = fault
			}
			reads := 0
			script.uploadQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if !strings.HasSuffix(statement, "FOR UPDATE") || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID {
					t.Error("任务读写未按当前范围加锁")
				}
				if mode == "task query failure" || (reads == 2 && mode == "readback failure") {
					return nil, fault
				}
				result := append([]driver.Value(nil), taskValues...)
				if mode == "missing task" || (reads == 2 && mode == "missing readback") {
					result = nil
				}
				if reads == 2 && result != nil {
					result[3], result[26] = strconv.FormatUint(newID, 10), writeTime
					switch mode {
					case "wrong object readback":
						result[3] = strconv.FormatUint(oldID, 10)
					case "wrong lease readback":
						result[18] = now.Add(2 * time.Minute)
					case "wrong version readback":
						result[19] = "4"
					case "old update time":
						result[26] = now.Add(-time.Microsecond)
					}
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.objectQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				// 2. 新旧对象均使用当前读，归属取自认证事务。
				if !strings.HasSuffix(statement, "FOR UPDATE") || len(arguments) != 3 || arguments[0].Value != actor.UserID {
					t.Error("暂存对象未按当前范围加锁")
				}
				old := arguments[1].Value == oldID
				if (old && mode == "old query failure") || (!old && mode == "new query failure") {
					return nil, fault
				}
				result := oldValues
				if !old {
					result = newValues
				}
				if (old && mode == "missing old") || (!old && mode == "missing new") {
					result = nil
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.leaseClockQuery = func(queryContext context.Context, _ string, arguments []driver.NamedValue) (driver.Rows, error) {
				if deadline, exists := queryContext.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second || len(arguments) != 0 {
					t.Error("替换时钟参数或期限错误")
				}
				if mode == "clock failure" {
					return nil, fault
				}
				result := []driver.Value{now}
				if mode == "missing clock" {
					result = nil
				}
				if mode == "invalid clock" {
					result[0] = now.Add(time.Nanosecond)
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.replaceUploadObject = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				expectedSQL := "UPDATE upload_tasks SET staged_object_id = ?, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_owner = ? AND lease_version = ? AND lease_until > UTC_TIMESTAMP(6) AND expires_at > UTC_TIMESTAMP(6) AND status = 'UPLOADING' AND upload_mode = 'SINGLE'"
				expected := []any{newID, actor.UserID, want.ID, oldID, owner, version}
				if statement != expectedSQL || len(arguments) != len(expected) {
					t.Fatal("暂存对象条件更新 SQL 或参数数量错误")
				}
				for index, argument := range arguments {
					if argument.Value != expected[index] {
						t.Error("暂存对象条件参数错误", index)
					}
				}
				switch mode {
				case "duplicate reference":
					return nil, duplicate
				case "wrapped duplicate":
					return nil, fmt.Errorf("driver: %w", duplicate)
				case "write failure":
					return nil, fault
				case "zero affected":
					return driver.RowsAffected(0), nil
				case "multiple affected":
					return driver.RowsAffected(2), nil
				}
				return driver.RowsAffected(1), nil
			}
			// 3. 核对完整结果，错误留给外层回滚。
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				oldArgument, ownerArgument, versionArgument := oldID, owner, version
				switch mode {
				case "wrong old object":
					oldArgument = 26
				case "wrong owner":
					ownerArgument = "stale-worker"
				case "wrong version":
					versionArgument = 2
				}
				actual, err := txStore.ReplaceUserUploadStagedObject(txContext, want.ID, oldArgument, newID, versionArgument, ownerArgument)
				if err != nil {
					if actual != nil {
						t.Error("替换失败交付了任务")
					}
					return err
				}
				expected := want
				expected.StagedObjectID, expected.UpdatedAt = newID, writeTime
				if !reflect.DeepEqual(actual, &expected) || actual.ExpectedSHA256 == want.ExpectedSHA256 {
					t.Error("替换结果的完整事实或摘要副本错误")
				}
				if mode == "outer failure" {
					return fault
				}
				return nil
			})
			if !errors.Is(err, wantError) || (wantError != nil && mode != "commit failure" && strings.Contains(script.sequence(), "commit")) {
				t.Fatal("暂存对象替换结果错误", err, script.sequence())
			}
		})
	}
}

func TestReplaceUserUploadStagedObjectBoundary(t *testing.T) {
	for _, mode := range []string{"ordinary store", "bound without transaction", "login transaction", "nil context", "zero task", "zero old", "zero new", "same object", "zero version", "empty owner", "long owner", "non ASCII owner", "wrong user", "zero device", "zero session", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				taskID, oldID, newID, version, owner := uint64(19), uint64(23), uint64(25), uint64(3), "current-worker"
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "zero task":
					taskID = 0
				case "zero old":
					oldID = 0
				case "zero new":
					newID = 0
				case "same object":
					newID, wantError = oldID, domain.ErrInvalidUploadTaskData
				case "zero version":
					version, wantError = 0, domain.ErrInvalidUploadTaskData
				case "empty owner":
					owner, wantError = "", domain.ErrInvalidUploadTaskData
				case "long owner":
					owner, wantError = strings.Repeat("a", 65), domain.ErrInvalidUploadTaskData
				case "non ASCII owner":
					owner, wantError = "执行者", domain.ErrInvalidUploadTaskData
				case "wrong user":
					txStore.loginUserID, wantError = 8, domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID, wantError = 0, domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID, wantError = 0, domain.ErrInvalidSessionData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				}
				task, err := txStore.ReplaceUserUploadStagedObject(ctx, taskID, oldID, newID, version, owner)
				if task != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Error("无效暂存对象替换调用未拒绝", err)
				}
				return err
			}
			var err error
			switch mode {
			case "ordinary store":
				err = action(context.Background(), store)
			case "bound without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: 7, authenticatedActor: &actor})
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, action)
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Contains(script.sequence(), "upload") || strings.Contains(script.sequence(), "object") || strings.Contains(script.sequence(), "lease_clock") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效调用进入任务或对象访问", err, script.sequence())
			}
		})
	}
}

func TestReplaceUserUploadStagedObjectCancellation(t *testing.T) {
	for _, phase := range []string{"task deadline", "old object deadline", "new object deadline", "clock deadline", "clock cancelled", "write cancelled", "write failure cancelled", "readback cancelled"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 逐阶段注入更短期限及取消，覆盖已更新后的读回。
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			_, taskValues, oldValues, newValues, now := uploadObjectReplacementFixture()
			reads := 0
			script.uploadQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				reads++
				if phase == "task deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if reads == 2 {
					cancel()
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: taskValues}}, nil
			}
			script.objectQuery = func(queryContext context.Context, _ string, arguments []driver.NamedValue) (driver.Rows, error) {
				old := arguments[1].Value == uint64(23)
				if (old && phase == "old object deadline") || (!old && phase == "new object deadline") {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				result := oldValues
				if !old {
					result = newValues
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.leaseClockQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "clock deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if phase == "clock cancelled" {
					cancel()
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			script.replaceUploadObject = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
				if phase == "write cancelled" || phase == "write failure cancelled" {
					cancel()
				}
				if phase == "write failure cancelled" {
					return nil, errors.New("private replacement cancellation")
				}
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				task, err := txStore.ReplaceUserUploadStagedObject(txContext, 19, 23, 25, 3, "current-worker")
				if task != nil {
					t.Error("取消后交付了任务")
				}
				return err
			})
			// 2. 原因穿透，外层回滚完成后结束验证。
			wantError := error(context.Canceled)
			if strings.HasSuffix(phase, "deadline") {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("替换取消结果或提交事件错误", err, script.sequence())
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("暂存对象替换取消后未回滚")
			}
		})
	}
}
