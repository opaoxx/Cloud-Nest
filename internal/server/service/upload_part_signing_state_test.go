package service

import (
	"crypto/sha256"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func uploadPartSigningTaskFixture(expiresAt time.Time) *domain.UploadTask {
	partSize := uint64(16 * 1024 * 1024)
	fileSize := partSize + 1
	digest := sha256.Sum256([]byte("fixture"))
	return &domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13,
		UploadMode: "MULTIPART", Status: domain.UploadStatusUploading, StorageUploadID: new("opaque+A/="),
		ExpectedSHA256: &digest, FileSize: fileSize, ReservedBytes: fileSize, PartSize: &partSize,
		PartCount: new(uint32(2)), ExpiresAt: expiresAt, LeaseVersion: 2,
	}
}

func TestValidateUploadPartSigningStateTimeBoundaries(t *testing.T) {
	databaseTime := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name      string
		expiresAt time.Time
		leaseEnds *time.Time
		want      error
	}{
		{name: "active with no lease", expiresAt: databaseTime.Add(time.Hour)},
		{name: "one microsecond before expiry", expiresAt: databaseTime.Add(time.Microsecond)},
		{name: "at expiry", expiresAt: databaseTime, want: domain.ErrUploadTaskNotReady},
		{name: "after expiry", expiresAt: databaseTime.Add(-time.Microsecond), want: domain.ErrUploadTaskNotReady},
		{name: "lease still valid", expiresAt: databaseTime.Add(time.Hour), leaseEnds: new(databaseTime.Add(time.Microsecond)), want: domain.ErrUploadTaskBusy},
		{name: "at lease expiry", expiresAt: databaseTime.Add(time.Hour), leaseEnds: &databaseTime},
		{name: "after lease expiry", expiresAt: databaseTime.Add(time.Hour), leaseEnds: new(databaseTime.Add(-time.Microsecond))},
		{name: "task expires during lease", expiresAt: databaseTime, leaseEnds: new(databaseTime.Add(time.Minute)), want: domain.ErrUploadTaskNotReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := uploadPartSigningTaskFixture(test.expiresAt)
			if test.leaseEnds != nil {
				task.LeaseOwner, task.LeaseUntil = new("fixture-worker"), test.leaseEnds
			}
			before := copyUploadTask(task)
			err := ValidateUploadPartSigningState(task, 7, 19, databaseTime)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(task, before) {
				t.Fatalf("判定错误或改变了任务，得到 %v，预期 %v", err, test.want)
			}
		})
	}
}

func TestValidateUploadPartSigningStateRejectsUnavailableAndInvalidTasks(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name   string
		change func(*domain.UploadTask)
		want   error
	}{
		{"other user", func(task *domain.UploadTask) { task.UserID++ }, domain.ErrInvalidUploadTaskData},
		{"other task", func(task *domain.UploadTask) { task.ID++ }, domain.ErrInvalidUploadTaskData},
		{"no staged object", func(task *domain.UploadTask) { task.StagedObjectID = 0 }, domain.ErrInvalidUploadTaskData},
		{"unknown mode", func(task *domain.UploadTask) { task.UploadMode = "UNKNOWN" }, domain.ErrInvalidUploadTaskData},
		{"unknown status", func(task *domain.UploadTask) { task.Status = "UNKNOWN" }, domain.ErrInvalidUploadTaskData},
		{"single", func(task *domain.UploadTask) { task.UploadMode = "SINGLE" }, domain.ErrUploadTaskNotReady},
		{"creating", func(task *domain.UploadTask) { task.Status = domain.UploadStatusCreating }, domain.ErrUploadTaskNotReady},
		{"completing", func(task *domain.UploadTask) { task.Status = domain.UploadStatusCompleting }, domain.ErrUploadTaskNotReady},
		{"verifying", func(task *domain.UploadTask) { task.Status = domain.UploadStatusVerifying }, domain.ErrUploadTaskNotReady},
		{"completed", func(task *domain.UploadTask) { task.Status = domain.UploadStatusCompleted }, domain.ErrUploadTaskNotReady},
		{"cancelling", func(task *domain.UploadTask) { task.Status = domain.UploadStatusCancelling }, domain.ErrUploadTaskNotReady},
		{"cancelled", func(task *domain.UploadTask) { task.Status = domain.UploadStatusCancelled }, domain.ErrUploadTaskNotReady},
		{"expiring", func(task *domain.UploadTask) { task.Status = domain.UploadStatusExpiring }, domain.ErrUploadTaskNotReady},
		{"expired", func(task *domain.UploadTask) { task.Status = domain.UploadStatusExpired }, domain.ErrUploadTaskNotReady},
		{"failed", func(task *domain.UploadTask) { task.Status = domain.UploadStatusFailed }, domain.ErrUploadTaskNotReady},
		{"missing session", func(task *domain.UploadTask) { task.StorageUploadID = nil }, domain.ErrUploadTaskNotReady},
		{"invalid session", func(task *domain.UploadTask) { task.StorageUploadID = new("bad\nsession") }, domain.ErrUploadTaskNotReady},
		{"missing part size", func(task *domain.UploadTask) { task.PartSize = nil }, domain.ErrInvalidUploadTaskData},
		{"missing part count", func(task *domain.UploadTask) { task.PartCount = nil }, domain.ErrInvalidUploadTaskData},
		{"inconsistent plan", func(task *domain.UploadTask) { task.PartCount = new(uint32(3)) }, domain.ErrInvalidUploadTaskData},
		{"missing checksum", func(task *domain.UploadTask) { task.ExpectedSHA256 = nil }, domain.ErrInvalidUploadTaskData},
		{"incomplete reservation", func(task *domain.UploadTask) { task.ReservedBytes-- }, domain.ErrInvalidUploadTaskData},
		{"already published", func(task *domain.UploadTask) { task.ResultEntryID = new(uint64(40)) }, domain.ErrInvalidUploadTaskData},
		{"invalid expiry", func(task *domain.UploadTask) { task.ExpiresAt = now.Add(time.Nanosecond) }, domain.ErrInvalidUploadTaskData},
		{"lease missing end", func(task *domain.UploadTask) { task.LeaseOwner = new("fixture-worker") }, domain.ErrInvalidUploadTaskData},
		{"lease missing owner", func(task *domain.UploadTask) { task.LeaseUntil = &now }, domain.ErrInvalidUploadTaskData},
		{"invalid lease time", func(task *domain.UploadTask) {
			task.LeaseOwner, task.LeaseUntil = new("fixture-worker"), new(now.Add(time.Nanosecond))
		}, domain.ErrInvalidUploadTaskData},
		{"invalid lease version", func(task *domain.UploadTask) {
			task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = new("fixture-worker"), &now, 0
		}, domain.ErrInvalidUploadTaskData},
		{"maximum version without active lease", func(task *domain.UploadTask) { task.LeaseVersion = math.MaxUint64 }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			test.change(task)
			if err := ValidateUploadPartSigningState(task, 7, 19, now); !errors.Is(err, test.want) {
				t.Fatalf("得到 %v，预期 %v", err, test.want)
			}
		})
	}
}

func TestValidateUploadPartSigningStateRejectsMissingInput(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	task := uploadPartSigningTaskFixture(now.Add(time.Hour))
	for _, test := range []struct {
		task           *domain.UploadTask
		userID, taskID uint64
		now            time.Time
	}{
		{nil, 7, 19, now},
		{task, 0, 19, now},
		{task, 7, 0, now},
		{task, 7, 19, time.Time{}},
		{task, 7, 19, now.Add(time.Nanosecond)},
	} {
		if err := ValidateUploadPartSigningState(test.task, test.userID, test.taskID, test.now); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatalf("无效输入未被拒绝，得到 %v", err)
		}
	}
}
