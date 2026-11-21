package domain

import (
	"errors"
	"testing"
	"time"
)

func TestShouldBeginUploadCancellationStatus(t *testing.T) {
	testUploadCancellationStatus(t, "SINGLE", ShouldBeginUploadCancellation)
}

func TestShouldBeginMultipartUploadCancellationStatus(t *testing.T) {
	testUploadCancellationStatus(t, "MULTIPART", ShouldBeginMultipartUploadCancellation)
}

func testUploadCancellationStatus(t *testing.T, uploadMode string, decide func(*UploadTask, time.Time) (bool, error)) {
	t.Helper()
	now := time.Date(2026, 11, 20, 8, 0, 0, 123456000, time.UTC)
	owner, leaseUntil := "upload-worker", now.Add(time.Second)
	for _, scenario := range []struct {
		status    UploadStatus
		wantBegin bool
		wantError error
	}{
		{status: UploadStatusCreating, wantBegin: true},
		{status: UploadStatusUploading, wantBegin: true},
		{status: UploadStatusCompleting, wantBegin: true},
		{status: UploadStatusVerifying, wantBegin: true},
		{status: UploadStatusCancelling},
		{status: UploadStatusCancelled},
		{status: UploadStatusCompleted},
		{status: UploadStatusExpired},
		{status: UploadStatusFailed},
		{status: UploadStatusExpiring, wantError: ErrUploadTaskNotReady},
		{status: "UNKNOWN", wantError: ErrInvalidUploadTaskData},
	} {
		t.Run(string(scenario.status), func(t *testing.T) {
			// 1. 以同一时刻检查十种持久状态和未知状态。
			task := UploadTask{UploadMode: uploadMode, Status: scenario.status, ExpiresAt: now.Add(time.Hour)}
			if uploadMode == "MULTIPART" && scenario.status != UploadStatusCreating {
				task.StorageUploadID = new("fixture-session")
			}
			if scenario.status.IsTerminal() || scenario.status == UploadStatusCancelling {
				// 已有结果的请求即使在任务到期或持有租约时，仍可读到当前状态。
				task.ExpiresAt = now.Add(-time.Second)
				task.LeaseOwner, task.LeaseUntil = &owner, &leaseUntil
			}
			if scenario.status == UploadStatusCompleted {
				task.ResultEntryID = new(uint64(42))
			}
			before := task
			shouldBegin, err := decide(&task, now)
			if shouldBegin != scenario.wantBegin || !errors.Is(err, scenario.wantError) {
				t.Fatalf("是否开始取消 = %t，错误 = %v，期望 %t、%v", shouldBegin, err, scenario.wantBegin, scenario.wantError)
			}
			if task != before || (task.ResultEntryID != nil && *task.ResultEntryID != 42) {
				t.Fatal("取消资格判定改变了任务或发布结果")
			}
		})
	}
}

func TestShouldBeginUploadCancellationTime(t *testing.T) {
	testUploadCancellationTime(t, "SINGLE", ShouldBeginUploadCancellation)
}

func TestShouldBeginMultipartUploadCancellationTime(t *testing.T) {
	testUploadCancellationTime(t, "MULTIPART", ShouldBeginMultipartUploadCancellation)
}

func testUploadCancellationTime(t *testing.T, uploadMode string, decide func(*UploadTask, time.Time) (bool, error)) {
	t.Helper()
	now := time.Date(2026, 11, 20, 8, 0, 0, 123456000, time.UTC)
	owner := "upload-worker"
	for _, scenario := range []struct {
		name        string
		expiryDelta time.Duration
		hasLease    bool
		leaseDelta  time.Duration
		wantBegin   bool
		wantError   error
	}{
		{name: "task still valid", expiryDelta: time.Microsecond, wantBegin: true},
		{name: "task expires exactly now", wantError: ErrUploadTaskNotReady},
		{name: "task expiration precedes active lease", expiryDelta: -time.Microsecond,
			hasLease: true, leaseDelta: time.Microsecond, wantError: ErrUploadTaskNotReady},
		{name: "lease still active", expiryDelta: time.Hour, hasLease: true, leaseDelta: time.Microsecond, wantError: ErrUploadTaskBusy},
		{name: "lease expires exactly now", expiryDelta: time.Hour, hasLease: true, wantBegin: true},
		{name: "lease already expired", expiryDelta: time.Hour, hasLease: true, leaseDelta: -time.Microsecond, wantBegin: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 用固定的微秒时钟比较任务和租约的到期边界。
			task := UploadTask{UploadMode: uploadMode, Status: UploadStatusUploading, ExpiresAt: now.Add(scenario.expiryDelta),
				FileSize: 12, ReservedBytes: 5, LeaseVersion: ^uint64(0)}
			offsetZone := time.FixedZone("test UTC+8", 8*60*60)
			until := now.In(offsetZone).Add(scenario.leaseDelta)
			if scenario.hasLease {
				task.LeaseOwner, task.LeaseUntil = &owner, &until
			}
			before, beforeOwner, beforeUntil := task, owner, until
			shouldBegin, err := decide(&task, now)
			if shouldBegin != scenario.wantBegin || !errors.Is(err, scenario.wantError) {
				t.Fatalf("是否开始取消 = %t，错误 = %v，期望 %t、%v", shouldBegin, err, scenario.wantBegin, scenario.wantError)
			}

			// 2. 只作判断，保留任务、预占和租约原值。
			if task != before || owner != beforeOwner || until != beforeUntil {
				t.Fatal("取消资格判定改变了任务数据")
			}
		})
	}
}

func TestShouldBeginUploadCancellationInvalidInputs(t *testing.T) {
	testUploadCancellationInvalidInputs(t, "SINGLE", "MULTIPART", ShouldBeginUploadCancellation)
}

func TestShouldBeginMultipartUploadCancellationInvalidInputs(t *testing.T) {
	testUploadCancellationInvalidInputs(t, "MULTIPART", "SINGLE", ShouldBeginMultipartUploadCancellation)
}

func testUploadCancellationInvalidInputs(t *testing.T, uploadMode, otherMode string, decide func(*UploadTask, time.Time) (bool, error)) {
	t.Helper()
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	expiry, invalidTime := now.Add(time.Hour), time.Time{}
	owner := "upload-worker"
	for _, scenario := range []struct {
		name      string
		task      *UploadTask
		clock     time.Time
		wantError error
	}{
		{name: "nil task", clock: now, wantError: ErrInvalidUploadTaskData},
		{name: "missing clock", task: &UploadTask{UploadMode: uploadMode, Status: UploadStatusUploading, ExpiresAt: expiry},
			wantError: ErrInvalidUploadTaskData},
		{name: "different mode terminal", task: &UploadTask{UploadMode: otherMode, Status: UploadStatusCompleted, ExpiresAt: expiry},
			clock: now, wantError: ErrUploadTaskNotReady},
		{name: "different mode still eligible", task: &UploadTask{UploadMode: otherMode, Status: UploadStatusUploading, ExpiresAt: expiry},
			clock: now, wantError: ErrUploadTaskNotReady},
		{name: "unknown mode", task: &UploadTask{UploadMode: "OTHER", Status: UploadStatusUploading, ExpiresAt: expiry},
			clock: now, wantError: ErrInvalidUploadTaskData},
		{name: "missing expiry", task: &UploadTask{UploadMode: uploadMode, Status: UploadStatusUploading},
			clock: now, wantError: ErrInvalidUploadTaskData},
		{name: "lease owner without deadline", task: &UploadTask{UploadMode: uploadMode, Status: UploadStatusUploading,
			ExpiresAt: expiry, LeaseOwner: &owner}, clock: now, wantError: ErrInvalidUploadTaskData},
		{name: "lease deadline without owner", task: &UploadTask{UploadMode: uploadMode, Status: UploadStatusUploading,
			ExpiresAt: expiry, LeaseUntil: &expiry}, clock: now, wantError: ErrInvalidUploadTaskData},
		{name: "missing lease clock", task: &UploadTask{UploadMode: uploadMode, Status: UploadStatusUploading,
			ExpiresAt: expiry, LeaseOwner: &owner, LeaseUntil: &invalidTime}, clock: now, wantError: ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 核对无效输入和模式不匹配的任务均不会进入写入分支。
			shouldBegin, err := decide(scenario.task, scenario.clock)
			if shouldBegin || !errors.Is(err, scenario.wantError) {
				t.Fatalf("是否开始取消 = %t，错误 = %v，期望 false、%v", shouldBegin, err, scenario.wantError)
			}
		})
	}
}
