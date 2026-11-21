package service

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestResolveUploadPartRenewalExpiryChoosesLaterDeadline(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{"extend one-hour task", time.Hour, 24 * time.Hour},
		{"extend just before expiry", time.Microsecond, 24 * time.Hour},
		{"keep equal deadline", 24 * time.Hour, 24 * time.Hour},
		{"keep later deadline", 25 * time.Hour, 25 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := uploadPartSigningTaskFixture(now.Add(test.remaining))
			account := domain.UserProfile{
				ID: task.UserID, Status: "ACTIVE", QuotaBytes: task.FileSize + 3, UsedBytes: 2, ReservedBytes: task.FileSize + 1,
			}
			taskBefore, accountBefore := copyUploadTask(task), account
			expectedExpiry := task.ExpiresAt.In(time.FixedZone("fixture", 8*3600))
			result, err := ResolveUploadPartRenewalExpiry(task, &account, expectedExpiry, now)
			if err != nil || !result.Equal(now.Add(test.want)) || result.Location() != time.UTC ||
				!reflect.DeepEqual(task, taskBefore) || account != accountBefore {
				t.Fatal("续期计时、额度边界或输入保留不符", result, err)
			}
		})
	}
}

func TestResolveUploadPartRenewalExpiryRejectsConflicts(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name       string
		changeTask func(*domain.UploadTask)
		changeUser func(*domain.UserProfile)
		want       error
	}{
		{name: "another account", changeUser: func(user *domain.UserProfile) { user.ID++ }, want: domain.ErrInvalidUploadTaskData},
		{name: "disabled account", changeUser: func(user *domain.UserProfile) { user.Status = "DISABLED" }, want: domain.ErrAccountDisabled},
		{name: "unknown account status", changeUser: func(user *domain.UserProfile) { user.Status = "UNKNOWN" }, want: domain.ErrInvalidAccountData},
		{name: "expired task", changeTask: func(task *domain.UploadTask) { task.ExpiresAt = now }, want: domain.ErrUploadTaskNotReady},
		{name: "not uploading", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCompleting }, want: domain.ErrUploadTaskNotReady},
		{name: "missing session", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = nil }, want: domain.ErrUploadTaskNotReady},
		{name: "active lease", changeTask: func(task *domain.UploadTask) {
			task.LeaseOwner, task.LeaseUntil = new("fixture-worker"), new(now.Add(time.Microsecond))
		}, want: domain.ErrUploadTaskBusy},
		{name: "incomplete task reservation", changeTask: func(task *domain.UploadTask) { task.ReservedBytes-- }, want: domain.ErrInvalidUploadTaskData},
		{name: "incomplete account reservation", changeUser: func(user *domain.UserProfile) { user.ReservedBytes-- }, want: domain.ErrUploadTaskNotReady},
		{name: "aggregate over quota", changeUser: func(user *domain.UserProfile) { user.UsedBytes++ }, want: domain.ErrQuotaExceeded},
		{name: "used bytes already over quota", changeUser: func(user *domain.UserProfile) { user.UsedBytes = user.QuotaBytes + 1 }, want: domain.ErrQuotaExceeded},
		{name: "sum would overflow", changeUser: func(user *domain.UserProfile) {
			user.QuotaBytes, user.UsedBytes = math.MaxUint64, math.MaxUint64
		}, want: domain.ErrQuotaExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			account := domain.UserProfile{ID: task.UserID, Status: "ACTIVE", QuotaBytes: task.FileSize + 10, UsedBytes: 10, ReservedBytes: task.FileSize}
			if test.changeTask != nil {
				test.changeTask(task)
			}
			if test.changeUser != nil {
				test.changeUser(&account)
			}
			result, err := ResolveUploadPartRenewalExpiry(task, &account, task.ExpiresAt, now)
			if !result.IsZero() || !errors.Is(err, test.want) {
				t.Fatal("前置条件未按预期拒绝", result, err, test.want)
			}
		})
	}
}

func TestResolveUploadPartRenewalExpiryRejectsStaleExpectedDeadline(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	task := uploadPartSigningTaskFixture(now.Add(time.Hour))
	account := domain.UserProfile{ID: task.UserID, Status: "ACTIVE", QuotaBytes: task.FileSize, ReservedBytes: task.FileSize}
	oldExpiry := task.ExpiresAt
	firstResult, err := ResolveUploadPartRenewalExpiry(task, &account, oldExpiry, now)
	if err != nil {
		t.Fatal(err)
	}
	task.ExpiresAt = firstResult
	for _, expected := range []time.Time{oldExpiry, firstResult.Add(time.Microsecond), time.Time{}} {
		result, err := ResolveUploadPartRenewalExpiry(task, &account, expected, now.Add(time.Second))
		if !result.IsZero() || !errors.Is(err, domain.ErrUploadTaskNotReady) {
			t.Fatal("过期、较新或缺失的旧期限仍可续期", result, err)
		}
	}
}

func TestResolveUploadPartRenewalExpiryRejectsOutOfRangeResult(t *testing.T) {
	now := time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC)
	task := uploadPartSigningTaskFixture(now.Add(time.Minute))
	account := domain.UserProfile{ID: task.UserID, Status: "ACTIVE", QuotaBytes: task.FileSize, ReservedBytes: task.FileSize}
	if result, err := ResolveUploadPartRenewalExpiry(task, &account, task.ExpiresAt, now); !result.IsZero() || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("返回了 MySQL 无法保存的日期", result, err)
	}
}
