package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type purgeProgressTargetReadFunc func(context.Context, uint64, uint64) (*domain.PurgeProgressTarget, error)

func (read purgeProgressTargetReadFunc) FindUserPurgeProgressTarget(ctx context.Context, userID, entryID uint64) (*domain.PurgeProgressTarget, error) {
	return read(ctx, userID, entryID)
}

func TestReadPurgeProgressTarget(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 8, SessionID: 9}
	parentID := uint64(11)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	finishedAt := startedAt.Add(time.Microsecond)
	for _, testCase := range []struct {
		name, status string
		finishedAt   *time.Time
	}{
		{name: "purging", status: "PURGING"},
		{name: "completed in the same microsecond", status: "PURGED", finishedAt: &startedAt},
		{name: "completed later", status: "PURGED", finishedAt: &finishedAt},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stored := domain.PurgeProgressTarget{
				ID: 23, UserID: actor.UserID, ParentID: &parentID, Status: testCase.status,
				PurgeStartedAt: &startedAt, PurgedAt: testCase.finishedAt,
			}
			// 1. 只查本次用户与目标，保留读取返回的清理状态。
			calls := 0
			reader := purgeProgressTargetReadFunc(func(ctx context.Context, userID, entryID uint64) (*domain.PurgeProgressTarget, error) {
				calls++
				if ctx != t.Context() || userID != actor.UserID || entryID != stored.ID {
					t.Fatal("读取未使用本次上下文、用户和目标")
				}
				return &stored, nil
			})
			result, err := ReadPurgeProgressTarget(t.Context(), actor, stored.ID, reader)
			if err != nil || calls != 1 || !reflect.DeepEqual(result, &stored) {
				t.Fatal("进度目标的返回数据或读取次数错误", result, calls, err)
			}

			// 2. 返回的可空字段有独立指针，可在下游独立使用。
			if result == &stored || result.ParentID == stored.ParentID || result.PurgeStartedAt == stored.PurgeStartedAt ||
				(result.PurgedAt != nil && result.PurgedAt == stored.PurgedAt) {
				t.Fatal("结果共享了读取目标的可变数据")
			}
		})
	}
}

func TestReadPurgeProgressTargetRejections(t *testing.T) {
	const entryID = 23
	actor := domain.Actor{UserID: 7, DeviceID: 8, SessionID: 9}
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	finishedAt := startedAt.Add(time.Microsecond)
	beforeStart := startedAt.Add(-time.Microsecond)
	outsideRange := time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
	subMicrosecond := startedAt.Add(time.Nanosecond)
	invalid, notFound := domain.ErrInvalidFileEntryData, domain.ErrFileEntryNotFound
	readFailure := errors.New("读取失败")
	for _, testCase := range []struct {
		name      string
		change    func(*domain.PurgeProgressTarget)
		readError error
		want      error
	}{
		{name: "missing or foreign target", readError: notFound, want: notFound},
		{name: "database failure", readError: readFailure, want: readFailure},
		{"real root", func(target *domain.PurgeProgressTarget) {
			target.ParentID, target.Status, target.PurgeStartedAt = nil, "ACTIVE", nil
		}, nil, notFound},
		{"active", func(target *domain.PurgeProgressTarget) { target.Status = "ACTIVE" }, nil, notFound},
		{"trashed", func(target *domain.PurgeProgressTarget) { target.Status = "TRASHED" }, nil, notFound},
		{"purged with ancestor", func(target *domain.PurgeProgressTarget) {
			target.Status, target.PurgeStartedAt, target.PurgedAt = "PURGED", nil, &finishedAt
		}, nil, notFound},
		{"wrong user", func(target *domain.PurgeProgressTarget) { target.UserID++ }, nil, invalid},
		{"wrong entry", func(target *domain.PurgeProgressTarget) { target.ID++ }, nil, invalid},
		{"self parent", func(target *domain.PurgeProgressTarget) { target.ParentID = &target.ID }, nil, invalid},
		{"unknown state", func(target *domain.PurgeProgressTarget) { target.Status = "UNKNOWN" }, nil, invalid},
		{"purging without start", func(target *domain.PurgeProgressTarget) { target.PurgeStartedAt = nil }, nil, invalid},
		{"purging with finish", func(target *domain.PurgeProgressTarget) { target.PurgedAt = &finishedAt }, nil, invalid},
		{"purged without finish", func(target *domain.PurgeProgressTarget) { target.Status = "PURGED" }, nil, invalid},
		{"purged before start", func(target *domain.PurgeProgressTarget) { target.Status, target.PurgedAt = "PURGED", &beforeStart }, nil, invalid},
		{"time outside database range", func(target *domain.PurgeProgressTarget) { target.PurgeStartedAt = &outsideRange }, nil, invalid},
		{"time beyond microsecond precision", func(target *domain.PurgeProgressTarget) { target.PurgeStartedAt = &subMicrosecond }, nil, invalid},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			parentID := uint64(11)
			target := domain.PurgeProgressTarget{ID: entryID, UserID: actor.UserID, ParentID: &parentID, Status: "PURGING", PurgeStartedAt: &startedAt}
			if testCase.change != nil {
				testCase.change(&target)
			}
			reader := purgeProgressTargetReadFunc(func(context.Context, uint64, uint64) (*domain.PurgeProgressTarget, error) {
				return &target, testCase.readError
			})
			result, err := ReadPurgeProgressTarget(t.Context(), actor, entryID, reader)
			if result != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("进度目标为 %+v，错误为 %v，期望 %v", result, err, testCase.want)
			}
		})
	}
}

func TestReadPurgeProgressTargetCancellation(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 8, SessionID: 9}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	reader := purgeProgressTargetReadFunc(func(context.Context, uint64, uint64) (*domain.PurgeProgressTarget, error) {
		calls++
		cancel()
		return nil, domain.ErrFileEntryNotFound
	})

	// 1. 身份或目标无效时，读取方法不执行。
	for _, invalidActor := range []domain.Actor{
		{DeviceID: actor.DeviceID, SessionID: actor.SessionID},
		{UserID: actor.UserID, SessionID: actor.SessionID},
		{UserID: actor.UserID, DeviceID: actor.DeviceID},
	} {
		if result, err := ReadPurgeProgressTarget(ctx, invalidActor, 23, reader); result != nil || err == nil {
			t.Fatal("缺少认证身份仍查询目标", result, err)
		}
	}
	if result, err := ReadPurgeProgressTarget(ctx, actor, 0, reader); result != nil || err == nil || calls != 0 {
		t.Fatal("无效参数触发查询", result, err, calls)
	}

	// 2. 读取中取消优先返回上下文错误，再次调用停止在读取之前。
	for range 2 {
		if result, err := ReadPurgeProgressTarget(ctx, actor, 23, reader); result != nil || !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatal("取消未阻止后续读取", result, err, calls)
		}
	}
}
