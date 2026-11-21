package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFindUserUploadTaskForPartSigningIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, values ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, values...); err != nil {
			t.Fatal(err)
		}
	}
	const userID, taskID = 7, 19
	const uploadID = "fixture+A/="
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	taskExpiry := now.Add(time.Hour)

	// 1. 固定专用连接的 MySQL 时间，使用正式领取及绑定入口准备任务。
	execute("SET timestamp = ?", now.Unix())
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("复位隔离连接的数据库时间失败", err)
		}
	})
	execute("UPDATE stored_objects SET file_size=16777217 WHERE id=23")
	execute(`UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,published_object_id=NULL,
		resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=16777217,part_size=16777216,
		part_count=2,reserved_bytes=16777217,lease_owner=NULL,lease_until=NULL,lease_version=3,
		next_attempt_at=NULL,expires_at=? WHERE id=?`, taskExpiry, taskID)
	candidates, err := store.ListMultipartUploadInitializationCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatal("初始化候选未就绪", candidates, err)
	}
	claimed, err := store.ClaimMultipartUploadInitializationLease(ctx, candidates[0], "fixture-worker")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindMultipartUploadSession(ctx, claimed, uploadID)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 交给资格判定，核对查询只读且只返回当前账号的任务及时间。
	check := func(at time.Time, want error) *domain.UploadTask {
		t.Helper()
		task, clock, err := store.FindUserUploadTaskForPartSigning(ctx, userID, taskID)
		if err != nil || task == nil || !clock.Equal(at) || clock.Location() != time.UTC {
			t.Fatal("未按同一查询交付当前任务和数据库 UTC 时间", clock, err)
		}
		if err := service.ValidateUploadPartSigningState(task, userID, taskID, clock); !errors.Is(err, want) {
			t.Fatal("数据库任务的签发资格不符", err, want)
		}
		current, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil || !reflect.DeepEqual(task, current) {
			t.Fatal("签发读取结果与普通读取不同", err)
		}
		return task
	}
	if actual := check(now, nil); !reflect.DeepEqual(bound, actual) {
		t.Fatal("已提交绑定状态未按原值读回")
	}
	for _, query := range [][2]uint64{{8, taskID}, {userID, 999}} {
		task, clock, err := store.FindUserUploadTaskForPartSigning(ctx, query[0], query[1])
		if task != nil || !clock.IsZero() || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("异主或缺失任务返回了查询数据", err)
		}
	}

	// 3. 调整隔离夹具时钟，验证租约和任务截止由数据库时间决定。
	leaseEnds := now.Add(time.Minute)
	execute("UPDATE upload_tasks SET lease_owner='fixture-completer',lease_until=? WHERE id=?", leaseEnds, taskID)
	check(now, domain.ErrUploadTaskBusy)
	execute("SET timestamp = ?", leaseEnds.Unix())
	check(leaseEnds, nil)
	execute("SET timestamp = ?", taskExpiry.Unix())
	check(taskExpiry, domain.ErrUploadTaskNotReady)
}
