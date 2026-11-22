package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func uploadTaskServiceFixture() (domain.Actor, domain.UploadTask) {
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	publishedID, resultID, partSize, partCount := uint64(29), uint64(31), uint64(5242880), uint32(3)
	resolvedName, storageID, leaseOwner := "发布笔记.MD", "storage-session", "worker-1"
	errorCode, errorMessage := "STORAGE_RETRY", "曾经重试"
	leaseUntil, nextAttemptAt, completedAt := stamp.Add(time.Minute), stamp.Add(2*time.Minute), stamp.Add(3*time.Minute)
	digest := sha256.Sum256([]byte("upload service fixture"))
	return domain.Actor{UserID: 7, SessionID: 17, DeviceID: 11}, domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, PublishedObjectID: &publishedID,
		TargetParentID: 13, RequestedName: "学习笔记.MD", ResolvedName: &resolvedName,
		UploadMode: "MULTIPART", StorageUploadID: &storageID, ExpectedSHA256: &digest,
		FileSize: 16777216, PartSize: &partSize, PartCount: &partCount, Status: domain.UploadStatusCompleted,
		ResultEntryID: &resultID, LeaseOwner: &leaseOwner, LeaseUntil: &leaseUntil, LeaseVersion: 3,
		NextAttemptAt: &nextAttemptAt, AttemptCount: 2, ExpiresAt: stamp.Add(24 * time.Hour),
		LastErrorCode: &errorCode, LastErrorMessage: &errorMessage,
		CreatedAt: stamp, UpdatedAt: stamp.Add(time.Second), CompletedAt: &completedAt,
	}
}

func TestNewUploadTaskService(t *testing.T) {
	// 1. 有效装配保存依赖，构造时查询调用数为零。
	calls := 0
	tasks := NewUploadTaskService(func(context.Context, uint64, uint64) (*domain.UploadTask, error) {
		calls++
		return nil, nil
	})
	if tasks == nil || calls != 0 {
		t.Fatal("任务服务装配或依赖调用时机错误")
	}
	// 2. 缺少查询依赖明确报告装配错误。
	defer func() {
		if recovered := recover(); recovered != "上传任务服务需要任务查询方法" {
			t.Fatal("缺失查询依赖未报告约定的装配错误")
		}
	}()
	NewUploadTaskService(nil)
}

func TestUploadTaskServiceResults(t *testing.T) {
	modes := []string{"full task", "another device", "nil optional SINGLE", "zero digest", "maximum unsigned", "expired retained"}
	for _, status := range []string{"CREATING", "UPLOADING", "COMPLETING", "VERIFYING", "COMPLETED", "CANCELLING", "CANCELLED", "EXPIRING", "EXPIRED", "FAILED"} {
		modes = append(modes, status)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备可信身份及已经由 Repository 检查的任务事实。
			actor, source := uploadTaskServiceFixture()
			switch mode {
			case "another device":
				actor.DeviceID = 15
			case "nil optional SINGLE":
				source = domain.UploadTask{ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13,
					RequestedName: "empty.bin", UploadMode: "SINGLE", Status: domain.UploadStatusUploading,
					ExpiresAt: source.ExpiresAt, CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt}
			case "zero digest":
				source.ExpectedSHA256 = new([sha256.Size]byte)
			case "maximum unsigned":
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				source.ID, source.UserID, source.DeviceID, source.StagedObjectID, source.TargetParentID = maximum, maximum, maximum, maximum, maximum
				*source.PublishedObjectID, *source.ResultEntryID, *source.PartSize, *source.PartCount = maximum, maximum, maximum, 10000
				source.FileSize, source.LeaseVersion, source.AttemptCount = maximum, maximum, ^uint32(0)
			case "expired retained":
				source.ExpiresAt = source.CreatedAt.Add(-24 * time.Hour)
			default:
				if domain.UploadStatus(mode).IsValid() {
					source.Status = domain.UploadStatus(mode)
				}
			}
			ctx := context.WithValue(context.Background(), struct{}{}, "upload context")
			calls := 0
			tasks := NewUploadTaskService(func(received context.Context, userID, taskID uint64) (*domain.UploadTask, error) {
				calls++
				if received != ctx || userID != actor.UserID || taskID != source.ID {
					t.Error("任务查询未收到原上下文、认证用户或目标 ID")
				}
				return &source, nil
			})
			// 2. 完整事实按值交付，来源设备及到期、终态原值保留。
			actual, err := tasks.Get(ctx, actor, source.ID)
			if err != nil || actual == &source || !reflect.DeepEqual(actual, &source) || calls != 1 {
				t.Fatal("任务读取结果或依赖调用次数错误", err)
			}
		})
	}
}

func TestUploadTaskServiceFailures(t *testing.T) {
	for _, mode := range []string{
		"nil task", "zero task id", "wrong task", "wrong owner", "zero source device", "unknown status",
		"not found", "repository data error", "query failure", "partial result with failure",
		"cancel with success", "cancel with missing", "cancel with failure", "cancel with invalid result",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备结果异常、查询故障或查询返回时取消。
			actor, source := uploadTaskServiceFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := errors.New("fixture task query failure")
			provided := &source
			var queryError error
			wantError := domain.ErrInvalidUploadTaskData
			switch mode {
			case "nil task":
				provided = nil
			case "zero task id":
				source.ID = 0
			case "wrong task":
				source.ID = 20
			case "wrong owner":
				source.UserID = 8
			case "zero source device":
				source.DeviceID = 0
			case "unknown status":
				source.Status = "UNKNOWN"
			case "not found", "cancel with missing":
				provided, queryError, wantError = nil, domain.ErrUploadTaskNotFound, domain.ErrUploadTaskNotFound
			case "repository data error":
				queryError, wantError = domain.ErrInvalidUploadTaskData, domain.ErrInvalidUploadTaskData
			case "query failure", "partial result with failure", "cancel with failure":
				queryError, wantError = fault, fault
				if mode == "query failure" {
					provided = nil
				}
			case "cancel with invalid result":
				provided = nil
			}
			cancelAfterRead := strings.HasPrefix(mode, "cancel ")
			if cancelAfterRead {
				wantError = context.Canceled
			}
			calls := 0
			tasks := NewUploadTaskService(func(context.Context, uint64, uint64) (*domain.UploadTask, error) {
				calls++
				if cancelAfterRead {
					cancel()
				}
				return provided, queryError
			})
			// 2. 所有故障整体返回 nil，错误原因可通过 errors.Is 识别。
			actual, err := tasks.Get(ctx, actor, 19)
			if actual != nil || !errors.Is(err, wantError) || calls != 1 {
				t.Fatal("任务服务错误原因、取消顺序或空结果错误", err)
			}
		})
	}
}

func TestUploadTaskServiceInput(t *testing.T) {
	actor, _ := uploadTaskServiceFixture()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	calls := 0
	tasks := NewUploadTaskService(func(context.Context, uint64, uint64) (*domain.UploadTask, error) {
		calls++
		return nil, nil
	})
	// 1. 上下文、三个身份标识和目标 ID 分别验证，提前取消保持原始原因。
	for _, fixture := range []struct {
		ctx       context.Context
		actor     domain.Actor
		taskID    uint64
		wantError error
	}{
		{actor: actor, taskID: 19},
		{ctx: context.Background(), actor: domain.Actor{DeviceID: 11, SessionID: 17}, taskID: 19},
		{ctx: context.Background(), actor: domain.Actor{UserID: 7, SessionID: 17}, taskID: 19},
		{ctx: context.Background(), actor: domain.Actor{UserID: 7, DeviceID: 11}, taskID: 19},
		{ctx: context.Background(), actor: actor},
		{ctx: cancelled, actor: actor, taskID: 19, wantError: context.Canceled},
		{ctx: expired, actor: actor, taskID: 19, wantError: context.DeadlineExceeded},
	} {
		actual, err := tasks.Get(fixture.ctx, fixture.actor, fixture.taskID)
		if actual != nil || err == nil || (fixture.wantError != nil && !errors.Is(err, fixture.wantError)) || calls != 0 {
			t.Fatal("无效身份或上下文调用了任务查询", err)
		}
	}
}

func TestUploadTaskServiceCopiesAndConcurrency(t *testing.T) {
	// 1. 两次交付的可空字段分别独立于来源和其他结果。
	actor, source := uploadTaskServiceFixture()
	source.ReplacementEntryID = new(uint64(31))
	source.ReplacementExpectedRevision = new(uint64(4))
	source.ReplacementPublishedRevision = new(uint64(5))
	_, expected := uploadTaskServiceFixture()
	expected.ReplacementEntryID = new(uint64(31))
	expected.ReplacementExpectedRevision = new(uint64(4))
	expected.ReplacementPublishedRevision = new(uint64(5))
	tasks := NewUploadTaskService(func(context.Context, uint64, uint64) (*domain.UploadTask, error) { return &source, nil })
	first, err := tasks.Get(context.Background(), actor, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tasks.Get(context.Background(), actor, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstValue, secondValue, sourceValue := reflect.ValueOf(first).Elem(), reflect.ValueOf(second).Elem(), reflect.ValueOf(source)
	for index := range firstValue.NumField() {
		field := firstValue.Field(index)
		if field.Kind() == reflect.Pointer && !field.IsNil() {
			if field.Pointer() == sourceValue.Field(index).Pointer() || field.Pointer() == secondValue.Field(index).Pointer() {
				t.Fatal("任务服务交付共享了可空字段")
			}
			field.Elem().Set(reflect.Zero(field.Type().Elem()))
		}
	}
	if !reflect.DeepEqual(source, expected) || !reflect.DeepEqual(second, &expected) {
		t.Fatal("修改第一份结果影响了来源或其他结果")
	}
	// 2. 多个请求共享只读来源，各自修改结果，来源始终保留原值。
	failures := make(chan error, 32)
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				result, err := tasks.Get(context.Background(), actor, source.ID)
				if err != nil || !reflect.DeepEqual(result, &expected) {
					failures <- fmt.Errorf("并发读取任务失败: %v", err)
					return
				}
				result.ExpectedSHA256[0] ^= 0xff
				*result.PublishedObjectID, *result.ResolvedName = 999, "changed"
			}
		}()
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	// 3. 来源在后续变化时，已经交付的第二份结果仍保持原值。
	source.ExpectedSHA256[0] ^= 0xff
	*source.ResolvedName, *source.PartSize, source.RequestedName = "source changed", 1, "source changed.bin"
	if !reflect.DeepEqual(second, &expected) {
		t.Fatal("来源后续变化影响了已交付任务")
	}

	// 4. 失败任务也应独立持有冲突时的版本。
	source.Status = domain.UploadStatusFailed
	source.ResultEntryID = nil
	source.ReplacementPublishedRevision = nil
	source.ReplacementConflictingRevision = new(uint64(6))
	source.LastErrorCode = new(domain.UploadRevisionConflictCode)
	conflicting, err := tasks.Get(context.Background(), actor, source.ID)
	if err != nil || conflicting == nil || conflicting.ReplacementConflictingRevision == nil ||
		*conflicting.ReplacementConflictingRevision != 6 {
		t.Fatal("查询服务没有保留冲突版本", err)
	}
	*conflicting.ReplacementConflictingRevision = 999
	if *source.ReplacementConflictingRevision != 6 {
		t.Fatal("修改冲突版本影响任务来源")
	}
}
