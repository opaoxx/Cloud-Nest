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

func TestLockUserUploadTaskResults(t *testing.T) {
	fault := errors.New("private task lock failure")
	for _, mode := range []string{"full record", "other source device", "maximum task ID", "missing", "query failure", "row failure", "wrong owner", "invalid digest", "unpaired lease", "outer failure", "commit failure"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备可信事务和完整任务，注入本组读取或提交结果。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			want, values := uploadTaskReadFixture()
			var queryError, rowsError, wantError error
			script := &loginTransactionScript{}
			switch mode {
			case "other source device":
				want.DeviceID, values[2] = 15, "15"
			case "maximum task ID":
				want.ID, values[0] = ^uint64(0), "18446744073709551615"
			case "missing":
				values, wantError = nil, domain.ErrUploadTaskNotFound
			case "query failure":
				queryError, wantError = fault, fault
			case "row failure":
				rowsError, wantError = fault, fault
			case "wrong owner":
				values[1], wantError = "8", domain.ErrInvalidUploadTaskData
			case "invalid digest":
				values[10], wantError = []byte{1}, domain.ErrInvalidUploadTaskData
			case "unpaired lease":
				values[18], wantError = nil, domain.ErrInvalidUploadTaskData
			case "outer failure":
				wantError = fault
			case "commit failure":
				script.commitError, wantError = fault, fault
			}
			store := revokeSessionTestStore(t, actor, script)
			queries := 0
			script.uploadQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				// 2. 完整投影、绑定范围、当前读、期限及查询独立性逐项核对。
				queries++
				expectedSQL := "SELECT " + strings.Join((&uploadTaskMappingRows{}).Columns(), ",") + " FROM upload_tasks WHERE user_id = ? AND id = ? LIMIT ?"
				if queries != 2 {
					expectedSQL += " FOR UPDATE"
				}
				if strings.ReplaceAll(statement, "`", "") != expectedSQL || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("任务锁投影、绑定参数或锁范围错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("任务锁查询期限错误")
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values, err: rowsError}}, queryError
			}
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				actual, err := txStore.LockUserUploadTask(ctx, want.ID)
				if queryError != nil || rowsError != nil || (wantError != nil && mode != "outer failure" && mode != "commit failure") {
					if actual != nil || !errors.Is(err, wantError) {
						t.Error("读取失败未返回空任务及原始错误", err)
					}
					return err
				}
				if err != nil || !reflect.DeepEqual(actual, &want) {
					t.Error("加锁读取未保留完整任务", err)
					return err
				}
				// 3. 改动返回副本，再交替普通/锁定读取，确认语句与结果独立。
				*actual.LeaseOwner, actual.ExpectedSHA256[0] = "changed", 0xff
				ordinary, err := txStore.FindUserUploadTask(ctx, actor.UserID, want.ID)
				if err != nil || !reflect.DeepEqual(ordinary, &want) {
					t.Error("任务副本或普通查询受到影响", err)
					return err
				}
				again, err := txStore.LockUserUploadTask(ctx, want.ID)
				if err != nil || !reflect.DeepEqual(again, &want) {
					t.Error("重复锁定读取异常", err)
					return err
				}
				if mode == "outer failure" {
					return fault
				}
				return nil
			})
			if (wantError == nil && err != nil) || (wantError != nil && !errors.Is(err, wantError)) {
				t.Fatal("任务锁事务结果错误", err)
			}
			if strings.Count(script.sequence(), "upload") != queries || queries == 0 || (wantError != nil && mode != "commit failure" && strings.Contains(script.sequence(), "commit")) {
				t.Fatal("任务锁生命周期错误", script.sequence())
			}
		})
	}
}

func TestLockUserUploadTaskBoundary(t *testing.T) {
	for _, mode := range []string{"ordinary store", "bound store without transaction", "login transaction", "nil context", "zero task", "wrong owner", "zero device", "zero session", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 对事务身份、参数和已失效上下文分别注入异常。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				taskID := uint64(19)
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "zero task":
					taskID = 0
				case "wrong owner":
					txStore.loginUserID, wantError = 8, domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID, wantError = 0, domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID, wantError = 0, domain.ErrInvalidSessionData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				case "expired":
					expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
					ctx, wantError = expired, context.DeadlineExceeded
				}
				task, err := txStore.LockUserUploadTask(ctx, taskID)
				if task != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Error("无效任务锁调用未拒绝", err)
				}
				return err
			}
			// 2. 验证只有已认证事务能进入任务查询。
			var err error
			switch mode {
			case "ordinary store":
				err = action(context.Background(), store)
			case "bound store without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: 7, authenticatedActor: &actor})
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, action)
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Contains(script.sequence(), "upload") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效调用进入任务查询或提交", err, script.sequence())
			}
		})
	}
}

func TestLockUserUploadTaskCancellation(t *testing.T) {
	for _, mode := range []string{"waiting deadline", "after query", "missing cancelled", "failure cancelled"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 在读取等待及查询返回时取消，验证上层可识别取消原因。
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			script.uploadQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if mode == "waiting deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				cancel()
				if mode == "failure cancelled" {
					return nil, errors.New("private cancelled lock")
				}
				_, values := uploadTaskReadFixture()
				if mode == "missing cancelled" {
					values = nil
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			// 2. 回调错误返回外层，实际 database/sql 事务完成回滚。
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				task, err := txStore.LockUserUploadTask(txContext, 19)
				if task != nil {
					t.Error("取消后交付了任务")
				}
				return err
			})
			wantError := error(context.Canceled)
			if mode == "waiting deadline" {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("任务锁取消原因或事务结果错误", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("任务锁取消后未回滚")
			}
		})
	}
}
