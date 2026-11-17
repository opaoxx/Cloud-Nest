package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadLeaseClockRows 提供数据库 UTC 微秒时间，复用单行及行错误处理。
type uploadLeaseClockRows struct{ *idempotencyRecordRows }

func (*uploadLeaseClockRows) Columns() []string { return []string{"UTC_TIMESTAMP(6)"} }

func TestClaimUserUploadContentLeaseResults(t *testing.T) {
	fault := errors.New("private lease failure")
	for _, mode := range []string{"initial", "released", "expired", "exact expiry", "zero bytes", "single limit", "maximum next version", "maximum task and object", "owner length limit", "clock timezone", "missing", "task query failure", "wrong stage", "oversize", "partial reservation", "wrong object", "wrong version", "task expired", "active lease", "exhausted", "clock failure", "missing clock", "zero clock", "clock precision", "clock overflow", "write failure", "zero affected", "multiple affected", "missing readback", "readback failure", "wrong owner readback", "wrong version readback", "wrong object readback", "wrong expiry readback", "old update time", "outer failure", "commit failure"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备任务、独立数据库时间与本组结果。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			expiry := time.Date(2026, 11, 18, 8, 0, 0, 123456000, time.UTC)
			want, values := uploadTaskCreateFixture(domain.CreateUploadInput{ParentID: 13, Name: "学习.bin", SizeBytes: 2048}, 23, expiry)
			databaseTime := want.CreatedAt.Add(time.Hour)
			clockValue := driver.Value(databaseTime)
			writeTime := databaseTime.Add(123 * time.Microsecond)
			owner := "fixture-new-worker"
			taskID, objectID, version := uint64(19), uint64(23), uint64(0)
			var wantError error
			script := &loginTransactionScript{}
			switch mode {
			case "initial":
			case "released":
				version = 3
			case "expired", "exact expiry", "active lease":
				version, values[17], values[18] = 3, "old-worker", databaseTime
				if mode == "expired" {
					values[18] = databaseTime.Add(-time.Microsecond)
				}
				if mode == "active lease" {
					values[18], wantError = databaseTime.Add(time.Microsecond), domain.ErrUploadTaskBusy
				}
			case "zero bytes":
				want.FileSize, want.ReservedBytes, values[11], values[16] = 0, 0, "0", "0"
			case "single limit":
				want.FileSize, want.ReservedBytes = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
				values[11], values[16] = "16777216", "16777216"
			case "maximum next version":
				version = ^uint64(0) - 1
			case "maximum task and object":
				taskID, objectID, want.ID, want.StagedObjectID = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
				values[0], values[3] = "18446744073709551615", "18446744073709551615"
			case "clock timezone":
				clockValue = databaseTime.In(time.FixedZone("fixture UTC+8", 8*3600))
			case "owner length limit":
				owner = strings.Repeat("a", 64)
			case "missing":
				wantError = domain.ErrUploadTaskNotFound
			case "task query failure", "clock failure", "write failure", "readback failure", "outer failure", "commit failure":
				wantError = fault
			case "missing clock":
				wantError = sql.ErrNoRows
			case "wrong stage":
				values[14], wantError = "VERIFYING", domain.ErrUploadTaskNotReady
			case "oversize":
				values[11], values[16], wantError = "16777217", "16777217", domain.ErrUploadFileTooLarge
			case "partial reservation":
				values[16], wantError = "2047", domain.ErrInvalidUploadTaskData
			case "wrong object", "wrong version":
				wantError = domain.ErrUploadTaskBusy
			case "task expired":
				values[22], wantError = databaseTime, domain.ErrUploadTaskNotReady
			case "exhausted":
				version, wantError = ^uint64(0), domain.ErrUploadLeaseVersionExhausted
			case "zero clock":
				clockValue, wantError = time.Time{}, domain.ErrInvalidUploadTaskData
			case "clock precision":
				clockValue, wantError = databaseTime.Add(time.Nanosecond), domain.ErrInvalidUploadTaskData
			case "clock overflow":
				clockValue, wantError = time.Date(9999, 12, 31, 23, 59, 30, 0, time.UTC), domain.ErrInvalidUploadTaskData
			case "zero affected":
				wantError = domain.ErrUploadTaskNotReady
			default:
				wantError = domain.ErrInvalidUploadTaskData
			}
			values[19] = strconv.FormatUint(version, 10)
			store := revokeSessionTestStore(t, actor, script)
			if mode == "commit failure" {
				script.commitError = fault
			}
			reads := 0
			script.uploadQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if !strings.HasSuffix(statement, "FOR UPDATE") || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != taskID {
					t.Error("领取前后未按当前范围加锁读取")
				}
				if mode == "task query failure" || (reads == 2 && mode == "readback failure") {
					return nil, fault
				}
				result := append([]driver.Value(nil), values...)
				if mode == "missing" || (reads == 2 && mode == "missing readback") {
					result = nil
				}
				if reads == 2 && result != nil {
					result[17], result[18], result[19], result[26] = owner, writeTime.Add(time.Minute), strconv.FormatUint(version+1, 10), writeTime
					switch mode {
					case "wrong owner readback":
						result[17] = "other-worker"
					case "wrong version readback":
						result[19] = strconv.FormatUint(version, 10)
					case "wrong object readback":
						result[3] = "24"
					case "wrong expiry readback":
						result[18] = writeTime.Add(59 * time.Second)
					case "old update time":
						result[26] = databaseTime.Add(-time.Microsecond)
					}
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.leaseClockQuery = func(queryContext context.Context, _ string, arguments []driver.NamedValue) (driver.Rows, error) {
				if len(arguments) != 0 {
					t.Error("数据库时间混入客户端参数")
				}
				if deadline, exists := queryContext.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("租约时间查询期限错误")
				}
				if mode == "clock failure" {
					return nil, fault
				}
				if mode == "missing clock" {
					return &uploadLeaseClockRows{&idempotencyRecordRows{}}, nil
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{clockValue}}}, nil
			}
			script.claimUploadLease = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				// 2. 锁定完整 SQL 和绑定值，期限及递增由数据库执行。
				expectedSQL := "UPDATE upload_tasks SET lease_owner = ?, lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND), lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND lease_version < ? AND status = 'UPLOADING' AND upload_mode = 'SINGLE' AND expires_at > UTC_TIMESTAMP(6) AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))"
				expected := []any{owner, actor.UserID, taskID, objectID, version, ^uint64(0)}
				if statement != expectedSQL || len(arguments) != len(expected) {
					t.Fatal("租约条件更新 SQL 或参数数量错误")
				}
				for index, argument := range arguments {
					if argument.Value != expected[index] {
						t.Error("租约绑定参数错误", index)
					}
				}
				if mode == "write failure" {
					return nil, fault
				}
				if mode == "zero affected" {
					return driver.RowsAffected(0), nil
				}
				if mode == "multiple affected" {
					return driver.RowsAffected(2), nil
				}
				return driver.RowsAffected(1), nil
			}
			// 3. 回调只交付当前事务结果，外层故障继续传播。
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				objectArgument, versionArgument := objectID, version
				if mode == "wrong object" {
					objectArgument++
				}
				if mode == "wrong version" {
					versionArgument++
				}
				actual, err := txStore.ClaimUserUploadContentLease(txContext, taskID, objectArgument, versionArgument, owner)
				if err != nil {
					if actual != nil {
						t.Error("领取失败交付了任务")
					}
					return err
				}
				until := writeTime.Add(time.Minute)
				want.LeaseOwner, want.LeaseUntil, want.LeaseVersion, want.UpdatedAt = &owner, &until, version+1, writeTime
				if !reflect.DeepEqual(actual, &want) {
					t.Error("领取结果未保留完整任务及新租约")
				}
				if mode == "outer failure" {
					return fault
				}
				return nil
			})
			if !errors.Is(err, wantError) || (wantError != nil && mode != "commit failure" && strings.Contains(script.sequence(), "commit")) {
				t.Fatal("领取事务结果错误", err, script.sequence())
			}
		})
	}
}

func TestClaimUserUploadContentLeaseBoundary(t *testing.T) {
	for _, mode := range []string{"ordinary store", "bound without transaction", "login transaction", "nil context", "zero task", "zero object", "wrong user", "zero device", "zero session", "empty owner", "long owner", "non ASCII owner", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 注入调用边界，校验拒绝发生在任务访问之前。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				taskID, objectID, owner := uint64(19), uint64(23), "fixture-new-worker"
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "zero task":
					taskID = 0
				case "zero object":
					objectID = 0
				case "wrong user":
					txStore.loginUserID, wantError = 8, domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID, wantError = 0, domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID, wantError = 0, domain.ErrInvalidSessionData
				case "empty owner":
					owner, wantError = "", domain.ErrInvalidUploadTaskData
				case "long owner":
					owner, wantError = strings.Repeat("a", 65), domain.ErrInvalidUploadTaskData
				case "non ASCII owner":
					owner, wantError = "执行者", domain.ErrInvalidUploadTaskData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				}
				task, err := txStore.ClaimUserUploadContentLease(ctx, taskID, objectID, 0, owner)
				if task != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Error("无效领取调用未拒绝", err)
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
			if err == nil || strings.Contains(script.sequence(), "upload") || strings.Contains(script.sequence(), "lease_clock") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效调用进入租约访问或提交", err, script.sequence())
			}
		})
	}
}

func TestClaimUserUploadContentLeaseCancellation(t *testing.T) {
	for _, phase := range []string{"clock deadline", "clock cancelled", "clock failure cancelled", "write deadline", "write cancelled", "write failure cancelled", "readback cancelled"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在时间查询、更新及读回阶段注入取消。
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			expiry := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
			want, values := uploadTaskCreateFixture(domain.CreateUploadInput{ParentID: 13, Name: "学习.bin", SizeBytes: 2048}, 23, expiry)
			now := want.CreatedAt.Add(time.Hour)
			reads := 0
			script.uploadQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				reads++
				if reads == 2 {
					cancel()
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.leaseClockQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "clock deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if strings.HasPrefix(phase, "clock") {
					cancel()
				}
				if phase == "clock failure cancelled" {
					return nil, errors.New("private clock cancellation")
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			script.claimUploadLease = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "write deadline" {
					<-writeContext.Done()
					return nil, writeContext.Err()
				}
				if phase == "write cancelled" || phase == "write failure cancelled" {
					cancel()
				}
				if phase == "write failure cancelled" {
					return nil, errors.New("private write cancellation")
				}
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				task, err := txStore.ClaimUserUploadContentLease(txContext, 19, 23, 0, "fixture-new-worker")
				if task != nil {
					t.Error("取消后交付了租约任务")
				}
				return err
			})
			// 2. 取消原因穿透，真实 database/sql 事务完成回滚。
			wantError := error(context.Canceled)
			if strings.HasSuffix(phase, "deadline") {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("租约取消原因或事务结果错误", err, script.sequence())
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("租约取消后未回滚")
			}
		})
	}
}
