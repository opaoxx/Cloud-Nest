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

func TestRenewUserUploadContentLeaseResults(t *testing.T) {
	fault := errors.New("fixture renewal failure")
	for _, fixture := range []struct {
		name   string
		want   error
		writes int
	}{
		{"extend", nil, 1},
		{"equal deadline", nil, 0},
		{"later deadline", nil, 0},
		{"wrong object", domain.ErrUploadTaskBusy, 0},
		{"wrong owner", domain.ErrUploadTaskBusy, 0},
		{"wrong version", domain.ErrUploadTaskBusy, 0},
		{"no lease", domain.ErrUploadTaskBusy, 0},
		{"wrong phase", domain.ErrUploadTaskNotReady, 0},
		{"lease exactly expired", domain.ErrUploadTaskBusy, 0},
		{"task exactly expired", domain.ErrUploadTaskNotReady, 0},
		{"clock failure", fault, 0},
		{"update failure", fault, 1},
		{"zero affected", domain.ErrUploadTaskBusy, 1},
		{"multiple affected", domain.ErrInvalidUploadTaskData, 1},
		{"unexpected readback", domain.ErrInvalidUploadTaskData, 1},
		{"cancel during update", context.Canceled, 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			task, values, _, _, now := uploadObjectReplacementFixture()
			objectID, version, owner := task.StagedObjectID, task.LeaseVersion, *task.LeaseOwner
			deadline := now.Add(30 * time.Second)
			switch fixture.name {
			case "equal deadline":
				deadline = now.Add(time.Minute)
			case "later deadline":
				deadline = now.Add(2 * time.Minute)
			case "wrong object":
				objectID++
			case "wrong owner":
				owner += "-old"
			case "wrong version":
				version--
			case "wrong phase":
				values[14] = "VERIFYING"
			case "lease exactly expired":
				deadline = now
			case "task exactly expired":
				values[22] = now
			}
			task.LeaseUntil, values[18] = &deadline, deadline
			if fixture.name == "no lease" {
				values[17], values[18] = nil, nil
			}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			script.uploadQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(statement, "FOR UPDATE") || arguments[0].Value != actor.UserID || arguments[1].Value != task.ID {
					t.Fatal("任务读取未使用当前读或用户范围")
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if fixture.name == "clock failure" {
					return nil, fault
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			writes := 0
			updatedAt := now.Add(time.Microsecond)
			renewedUntil := updatedAt.Add(time.Minute)
			script.renewUploadLease = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				for _, condition := range []string{
					"user_id = ?", "staged_object_id = ?", "lease_version = ?", "lease_owner = ?",
					"expires_at > UTC_TIMESTAMP(6)", "lease_until > UTC_TIMESTAMP(6)", "lease_until < DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND)",
				} {
					if !strings.Contains(statement, condition) {
						t.Errorf("续租缺少条件 %s", condition)
					}
				}
				got := make([]any, len(arguments))
				for index, argument := range arguments {
					got[index] = argument.Value
				}
				if !reflect.DeepEqual(got, []any{actor.UserID, task.ID, objectID, version, owner}) {
					t.Error("续租绑定范围错误", got)
				}
				switch fixture.name {
				case "update failure":
					return nil, fault
				case "zero affected":
					return driver.RowsAffected(0), nil
				case "multiple affected":
					return driver.RowsAffected(2), nil
				case "cancel during update":
					cancel()
				}
				values[18], values[26] = renewedUntil, updatedAt
				if fixture.name == "unexpected readback" {
					values[19] = "4"
				}
				return driver.RowsAffected(1), nil
			}
			var result *domain.UploadTask
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				var err error
				result, err = txStore.RenewUserUploadContentLease(txContext, task.ID, objectID, version, owner)
				return err
			})
			if !errors.Is(err, fixture.want) || writes != fixture.writes {
				t.Fatal("续租结果或写入次数错误", err, writes)
			}
			if err == nil {
				if writes == 1 {
					task.LeaseUntil, task.UpdatedAt = &renewedUntil, updatedAt
				}
				if !reflect.DeepEqual(result, &task) {
					t.Fatal("续租改变了期限及更新时间以外的事实")
				}
			} else if result != nil {
				t.Fatal("续租失败仍返回任务")
			}
			if errors.Is(err, context.Canceled) {
				select {
				case <-script.rolledBack:
				case <-time.After(5 * time.Second):
					t.Fatal("取消后未完成回滚")
				}
			}
			if err != nil && !strings.HasSuffix(script.sequence(), ",rollback") {
				t.Fatal("续租失败未回滚", script.sequence())
			}
		})
	}
}

func TestRenewUserUploadContentLeaseInputs(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	store := revokeSessionTestStore(t, actor, &loginTransactionScript{})
	for _, fixture := range []struct {
		store                 *Store
		ctx                   context.Context
		task, object, version uint64
		owner                 string
	}{
		{nil, context.Background(), 19, 23, 3, "worker"},
		{store, nil, 19, 23, 3, "worker"},
		{store, context.Background(), 0, 23, 3, "worker"},
		{store, context.Background(), 19, 0, 3, "worker"},
		{store, context.Background(), 19, 23, 0, "worker"},
		{store, context.Background(), 19, 23, 3, ""},
		{store, context.Background(), 19, 23, 3, strings.Repeat("x", 65)},
		{store, context.Background(), 19, 23, 3, "上传"},
		{store, context.Background(), 19, 23, 3, "worker"},
	} {
		if task, err := fixture.store.RenewUserUploadContentLease(fixture.ctx, fixture.task, fixture.object, fixture.version, fixture.owner); task != nil || err == nil {
			t.Fatal("无效续租参数或事务外调用被接受", fixture)
		}
	}
}
