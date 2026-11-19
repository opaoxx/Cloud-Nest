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

func TestClaimUploadVerificationLeaseResults(t *testing.T) {
	const owner = "verification-worker"
	fault := errors.New("fixture claim failure")
	for _, fixture := range []struct {
		name string
		want error
	}{
		{"first claim", nil},
		{"old lease exactly due", nil},
		{"wrong phase", domain.ErrUploadTaskNotReady},
		{"wrong object", domain.ErrUploadTaskBusy},
		{"stale version", domain.ErrUploadTaskBusy},
		{"exhausted version", domain.ErrUploadLeaseVersionExhausted},
		{"task exactly expired", domain.ErrUploadTaskNotReady},
		{"retry not due", domain.ErrUploadTaskNotReady},
		{"lease still active", domain.ErrUploadTaskBusy},
		{"wrong object phase", domain.ErrInvalidStoredObjectData},
		{"wrong object size", domain.ErrInvalidStoredObjectData},
		{"disabled account", domain.ErrAccountDisabled},
		{"clock failure", fault},
		{"no affected row", domain.ErrUploadTaskBusy},
		{"invalid readback", domain.ErrInvalidUploadTaskData},
		{"commit failure", fault},
		{"cancel during update", context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task, values, objectValues, _, now := uploadObjectReplacementFixture()
			task.Status, task.LeaseOwner, task.LeaseUntil = domain.UploadStatusVerifying, nil, nil
			values[14], values[17], values[18], objectValues[8] = string(task.Status), nil, nil, "VERIFYING"
			candidate := domain.UploadVerificationCandidate{TaskID: task.ID, UserID: task.UserID, StagedObjectID: task.StagedObjectID, LeaseVersion: task.LeaseVersion}
			script := &loginTransactionScript{status: "ACTIVE"}
			switch fixture.name {
			case "old lease exactly due":
				values[17], values[18], values[20], task.NextAttemptAt = "old-worker", now, now, &now
			case "wrong phase":
				values[14] = "UPLOADING"
			case "wrong object":
				candidate.StagedObjectID++
			case "stale version":
				candidate.LeaseVersion--
			case "exhausted version":
				candidate.LeaseVersion, values[19] = ^uint64(0), "18446744073709551615"
			case "task exactly expired":
				values[22] = now
			case "retry not due":
				values[20] = now.Add(time.Microsecond)
			case "lease still active":
				values[17], values[18] = "old-worker", now.Add(time.Microsecond)
			case "wrong object phase":
				objectValues[8] = "STAGING"
			case "wrong object size":
				objectValues[6] = int64(0)
			case "disabled account":
				script.status = "DISABLED"
			case "commit failure":
				script.commitError = fault
			}
			store := loginTransactionTestStore(t, script)
			script.uploadQuery = func(_ context.Context, statement string, _ []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(statement, "FOR UPDATE") {
					t.Error("任务读取未加锁")
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.objectQuery = func(_ context.Context, statement string, _ []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(statement, "FOR UPDATE") {
					t.Error("对象读取未加锁")
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: objectValues}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if fixture.name == "clock failure" {
					return nil, fault
				}
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			leaseUntil := now.Add(time.Minute)
			script.claimUploadLease = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				for _, clause := range []string{"status = 'VERIFYING'", "next_attempt_at <= UTC_TIMESTAMP(6)", "lease_until <= UTC_TIMESTAMP(6)", "expires_at > UTC_TIMESTAMP(6)", "staged_object_id = ?", "lease_version = ?"} {
					if !strings.Contains(statement, clause) {
						t.Error("校验租约更新缺少条件", clause)
					}
				}
				actual := make([]any, len(arguments))
				for index, argument := range arguments {
					actual[index] = argument.Value
				}
				if !reflect.DeepEqual(actual, []any{owner, candidate.UserID, candidate.TaskID, candidate.StagedObjectID, candidate.LeaseVersion, ^uint64(0)}) {
					t.Error("校验租约更新绑定错误", actual)
				}
				switch fixture.name {
				case "no affected row":
					return driver.RowsAffected(0), nil
				case "cancel during update":
					cancel()
				}
				values[17], values[18], values[19], values[26] = owner, leaseUntil, "4", now
				if fixture.name == "invalid readback" {
					values[17] = "other-worker"
				}
				return driver.RowsAffected(1), nil
			}
			got, err := store.ClaimUploadVerificationLease(ctx, candidate, owner)
			if !errors.Is(err, fixture.want) {
				t.Fatal("领取结果错误", err)
			}
			if fixture.want == nil {
				task.LeaseOwner, task.LeaseUntil, task.LeaseVersion, task.UpdatedAt = new(owner), &leaseUntil, 4, now
				if !reflect.DeepEqual(got, &task) || !strings.HasSuffix(script.sequence(), ",commit") {
					t.Fatal("提交结果或事务顺序错误", got, script.sequence())
				}
			} else if got != nil {
				t.Fatal("失败仍返回了已领取任务")
			}
			if fixture.want == context.Canceled {
				select {
				case <-script.rolledBack:
				case <-time.After(5 * time.Second):
					t.Fatal("取消后未完成回滚")
				}
			}
			if fixture.want != nil && fixture.name != "commit failure" && !strings.HasSuffix(script.sequence(), ",rollback") {
				t.Fatal("失败没有回滚", script.sequence())
			}
		})
	}
}

func TestClaimUploadVerificationLeaseInputs(t *testing.T) {
	store := loginTransactionTestStore(t, &loginTransactionScript{status: "ACTIVE"})
	normal := domain.UploadVerificationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23}
	for _, fixture := range []struct {
		store     *Store
		ctx       context.Context
		candidate domain.UploadVerificationCandidate
		owner     string
	}{
		{nil, context.Background(), normal, "worker"},
		{&Store{}, context.Background(), normal, "worker"},
		{store, nil, normal, "worker"},
		{store, context.Background(), domain.UploadVerificationCandidate{}, "worker"},
		{store, context.Background(), normal, ""},
		{store, context.Background(), normal, strings.Repeat("x", 65)},
		{store, context.Background(), normal, "上传"},
	} {
		if task, err := fixture.store.ClaimUploadVerificationLease(fixture.ctx, fixture.candidate, fixture.owner); task != nil || err == nil {
			t.Fatal("不合法的校验租约调用被接受")
		}
	}
	if err := store.WithLoginUserTransaction(context.Background(), normal.UserID, func(ctx context.Context, txStore *Store) error {
		if task, err := txStore.ClaimUploadVerificationLease(ctx, normal, "worker"); task != nil || err == nil {
			t.Error("已打开的事务里接受了嵌套领取")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
