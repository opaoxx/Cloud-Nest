package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func uploadContentStateFixture() (domain.UploadTask, time.Time) {
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	digest := sha256.Sum256([]byte("content state fixture"))
	return domain.UploadTask{ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13, RequestedName: "学习.bin", UploadMode: "SINGLE", ExpectedSHA256: &digest, FileSize: 2048, ReservedBytes: 2048, Status: domain.UploadStatusUploading, CreatedAt: stamp, UpdatedAt: stamp, ExpiresAt: stamp.Add(24 * time.Hour)}, stamp.Add(time.Hour)
}

func TestValidateUploadContentStateResults(t *testing.T) {
	for _, mode := range []string{"ordinary", "zero bytes", "single limit", "maximum IDs", "deadline minus nanosecond", "timezone", "minimum year", "maximum year", "no digest", "zero digest", "existing lease facts"} {
		t.Run(mode, func(t *testing.T) {
			task, now := uploadContentStateFixture()
			switch mode {
			case "zero bytes":
				task.FileSize, task.ReservedBytes = 0, 0
			case "single limit":
				task.FileSize, task.ReservedBytes = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
			case "maximum IDs":
				task.ID, task.UserID, task.DeviceID, task.StagedObjectID, task.TargetParentID = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
			case "deadline minus nanosecond":
				now = task.ExpiresAt.Add(-time.Nanosecond)
			case "timezone":
				zone := time.FixedZone("fixture UTC+8", 8*3600)
				task.ExpiresAt, now = task.ExpiresAt.In(zone), now.In(zone)
			case "minimum year":
				task.ExpiresAt = time.Date(1000, 1, 2, 0, 0, 0, 0, time.UTC)
				now = task.ExpiresAt.Add(-time.Hour)
			case "maximum year":
				task.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "no digest":
				task.ExpectedSHA256 = nil
			case "zero digest":
				task.ExpectedSHA256 = new([sha256.Size]byte)
			case "existing lease facts":
				owner := "fixture-worker"
				until := now.Add(time.Minute)
				task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, 42
			}
			before := task
			if err := ValidateUploadContentState(&task, task.UserID, task.ID, now); err != nil || !reflect.DeepEqual(task, before) {
				t.Fatal("合法接收阶段或来源保护错误", err)
			}
		})
	}
}

func TestValidateUploadContentStateInvalidData(t *testing.T) {
	for _, mode := range []string{"nil record", "zero record ID", "wrong task ID", "wrong user", "zero device", "zero object", "zero parent", "unknown mode", "lowercase mode", "unknown status", "lowercase status", "zero expiry", "year below", "year above", "UTC year overflow", "short reservation", "excess reservation", "storage session", "part size", "part count", "published object", "resolved name", "result entry", "completed time"} {
		t.Run(mode, func(t *testing.T) {
			task, now := uploadContentStateFixture()
			input := &task
			switch mode {
			case "nil record":
				input = nil
			case "zero record ID":
				task.ID = 0
			case "wrong task ID":
				task.ID++
			case "wrong user":
				task.UserID++
			case "zero device":
				task.DeviceID = 0
			case "zero object":
				task.StagedObjectID = 0
			case "zero parent":
				task.TargetParentID = 0
			case "unknown mode":
				task.UploadMode = "private-mode"
			case "lowercase mode":
				task.UploadMode = "single"
			case "unknown status":
				task.Status = "private-status"
			case "lowercase status":
				task.Status = "uploading"
			case "zero expiry":
				task.ExpiresAt = time.Time{}
			case "year below":
				task.ExpiresAt = time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)
			case "year above":
				task.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "UTC year overflow":
				task.ExpiresAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
			case "short reservation":
				task.ReservedBytes--
			case "excess reservation":
				task.ReservedBytes++
			case "storage session":
				task.StorageUploadID = new(string)
			case "part size":
				task.PartSize = new(uint64)
			case "part count":
				task.PartCount = new(uint32)
			case "published object":
				task.PublishedObjectID = new(uint64)
			case "resolved name":
				task.ResolvedName = new(string)
			case "result entry":
				task.ResultEntryID = new(uint64)
			case "completed time":
				task.CompletedAt = new(time.Time)
			}
			err := ValidateUploadContentState(input, 7, 19, now)
			if !errors.Is(err, domain.ErrInvalidUploadTaskData) || err.Error() != "上传任务数据异常" {
				t.Fatal("异常任务应返回固定数据错误", err)
			}
		})
	}
	for _, size := range []uint64{domain.MaxSingleUploadSizeBytes + 1, 9007199254740992, ^uint64(0)} {
		t.Run("oversize "+strconv.FormatUint(size, 10), func(t *testing.T) {
			task, now := uploadContentStateFixture()
			task.FileSize, task.ReservedBytes = size, size
			if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadFileTooLarge) {
				t.Fatal("完整声明大小未按接收能力检查", err)
			}
		})
	}
}

func TestValidateUploadContentStateNotReady(t *testing.T) {
	for _, status := range []domain.UploadStatus{domain.UploadStatusCreating, domain.UploadStatusCompleting, domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling, domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			task, now := uploadContentStateFixture()
			task.Status = status
			if status.IsTerminal() {
				task.ReservedBytes = 0
			}
			if status == domain.UploadStatusCompleted {
				published, entry, completed := uint64(29), uint64(31), now
				task.PublishedObjectID, task.ResultEntryID, task.CompletedAt = &published, &entry, &completed
			}
			if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("其他有效阶段未返回任务未就绪", err)
			}
		})
	}
	t.Run("MULTIPART", func(t *testing.T) {
		task, now := uploadContentStateFixture()
		size, count, storageID := uint64(5*1024*1024), uint32(1), "fixture-multipart"
		task.UploadMode, task.PartSize, task.PartCount, task.StorageUploadID = "MULTIPART", &size, &count, &storageID
		if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadTaskNotReady) {
			t.Fatal("已知其他上传方式未返回未就绪", err)
		}
	})
	for _, offset := range []time.Duration{0, time.Nanosecond, 24 * time.Hour} {
		t.Run("expiry "+offset.String(), func(t *testing.T) {
			task, _ := uploadContentStateFixture()
			err := ValidateUploadContentState(&task, 7, 19, task.ExpiresAt.Add(offset))
			if !errors.Is(err, domain.ErrUploadTaskNotReady) || err.Error() != "上传任务当前不可接收文件内容" {
				t.Fatal("到期边界或固定提示错误", err)
			}
		})
	}
}

func TestValidateUploadContentStateInputsAndOrder(t *testing.T) {
	task, now := uploadContentStateFixture()
	for _, input := range []struct {
		userID, taskID uint64
		now            time.Time
	}{{0, 19, now}, {7, 0, now}, {7, 19, time.Time{}}} {
		err := ValidateUploadContentState(&task, input.userID, input.taskID, input.now)
		if err == nil || errors.Is(err, domain.ErrUploadTaskNotReady) || errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("无效调用未作为调用错误处理", err)
		}
	}
	task.ExpiresAt = now
	task.UserID = 8
	if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("范围应先于期限检查", err)
	}
	task.UserID = 7
	task.FileSize = domain.MaxSingleUploadSizeBytes + 1
	if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("期限应先于接收声明检查", err)
	}
	task.ExpiresAt = now.Add(time.Hour)
	task.Status = domain.UploadStatusVerifying
	if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("阶段应先于接收声明检查", err)
	}
	task.Status = domain.UploadStatusUploading
	if err := ValidateUploadContentState(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadFileTooLarge) {
		t.Fatal("大小应先于预占一致性检查", err)
	}
}

func TestValidateUploadContentStateIndependent(t *testing.T) {
	task, now := uploadContentStateFixture()
	owner, diagnostic := "fixture-worker", "private-diagnostic"
	until := now.Add(time.Minute)
	task.LeaseOwner, task.LeaseUntil, task.LeaseVersion, task.LastErrorMessage = &owner, &until, 42, &diagnostic
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var original domain.UploadTask
	if err := json.Unmarshal(body, &original); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				if err := ValidateUploadContentState(&task, 7, 19, now); err != nil {
					t.Error("并发接收状态校验失败", err)
				}
			}
		}()
	}
	workers.Wait()
	if !reflect.DeepEqual(task, original) {
		t.Fatal("接收状态校验修改了来源记录、摘要或租约")
	}
}
