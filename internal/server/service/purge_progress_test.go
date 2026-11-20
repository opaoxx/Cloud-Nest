package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type purgeProgressReaderStub struct {
	purgeProgressTargetReadFunc
	listSubtree func(context.Context, uint64, uint64) ([]domain.PurgePreviewSubtreeEntry, error)
}

func (reader purgeProgressReaderStub) ListUserPurgePreviewSubtree(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
	return reader.listSubtree(ctx, userID, entryID)
}

func TestPurgeProgressServiceGet(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 8, SessionID: 9}
	parentID := uint64(11)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	finishedAt := startedAt.Add(time.Microsecond)
	for _, scenario := range []struct {
		status      string
		finishedAt  *time.Time
		wantCount   int
		wantActions []string
	}{
		{"PURGING", nil, 2, []string{"begin", "target", "subtree", "finish"}},
		{"PURGED", &finishedAt, 0, []string{"begin", "target", "finish"}},
	} {
		t.Run(scenario.status, func(t *testing.T) {
			ctx := t.Context()
			snapshotContext, cancel := context.WithCancel(ctx)
			defer cancel()
			var actions []string
			reader := purgeProgressReaderStub{
				purgeProgressTargetReadFunc: func(readContext context.Context, userID, entryID uint64) (*domain.PurgeProgressTarget, error) {
					actions = append(actions, "target")
					if readContext != snapshotContext || userID != actor.UserID || entryID != 23 {
						t.Fatal("目标读取使用了错误的快照或查询范围")
					}
					return &domain.PurgeProgressTarget{
						ID: entryID, UserID: userID, ParentID: &parentID, Status: scenario.status,
						PurgeStartedAt: &startedAt, PurgedAt: scenario.finishedAt,
					}, nil
				},
				listSubtree: func(readContext context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
					actions = append(actions, "subtree")
					if readContext != snapshotContext || userID != actor.UserID || entryID != 23 {
						t.Fatal("子树读取使用了错误的快照或查询范围")
					}
					return []domain.PurgePreviewSubtreeEntry{
						{ID: 23, EntryType: "DIR", Status: "PURGING", Revision: 2},
						{ID: 24, EntryType: "FILE", Status: "TRASHED", Revision: 1},
						{ID: 25, EntryType: "FILE", Status: "PURGED", Revision: 2},
					}, nil
				},
			}
			progressService := NewPurgeProgressService(func(requestContext context.Context, read func(context.Context, PurgeProgressReader) error) error {
				actions = append(actions, "begin")
				if requestContext != ctx {
					t.Fatal("快照入口未接收原请求上下文")
				}
				if err := read(snapshotContext, reader); err != nil {
					return err
				}
				actions = append(actions, "finish")
				return nil
			})
			result, err := progressService.Get(ctx, actor, 23)
			want := domain.PurgeProgress{EntryID: 23, Status: scenario.status, RemainingCount: scenario.wantCount}
			if err != nil || result == nil || *result != want || !slices.Equal(actions, scenario.wantActions) {
				t.Fatal("进度或读取顺序不符", result, actions, err)
			}
		})
	}
}

func TestPurgeProgressServiceGetFailures(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 8, SessionID: 9}
	parentID := uint64(11)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	finishFailure := errors.New("快照结束失败")
	subtreeFailure := errors.New("子树读取失败")
	invalid := domain.ErrInvalidFileEntryData
	notFound := domain.ErrFileEntryNotFound
	for _, scenario := range []struct {
		name                                   string
		targetError, subtreeError, finishError error
		missingRoot, skipRead, cancelOnSubtree bool
		want                                   error
		wantSubtreeReads                       int
	}{
		{name: "missing target", targetError: notFound, want: notFound},
		{name: "missing subtree for known target", subtreeError: notFound, want: invalid, wantSubtreeReads: 1},
		{name: "oversized subtree", subtreeError: domain.ErrFileEntrySubtreeTooLarge, want: domain.ErrFileEntrySubtreeTooLarge, wantSubtreeReads: 1},
		{name: "database failure", subtreeError: subtreeFailure, want: subtreeFailure, wantSubtreeReads: 1},
		{name: "incomplete subtree", missingRoot: true, want: invalid, wantSubtreeReads: 1},
		{name: "snapshot finish failure", finishError: finishFailure, want: finishFailure, wantSubtreeReads: 1},
		{name: "callback never ran", skipRead: true, want: invalid},
		{name: "cancel during subtree read", cancelOnSubtree: true, want: context.Canceled, wantSubtreeReads: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			subtreeReads := 0
			reader := purgeProgressReaderStub{
				purgeProgressTargetReadFunc: func(context.Context, uint64, uint64) (*domain.PurgeProgressTarget, error) {
					return &domain.PurgeProgressTarget{
						ID: 23, UserID: actor.UserID, ParentID: &parentID, Status: "PURGING", PurgeStartedAt: &startedAt,
					}, scenario.targetError
				},
				listSubtree: func(context.Context, uint64, uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
					subtreeReads++
					if scenario.cancelOnSubtree {
						cancel()
					}
					if scenario.missingRoot {
						return []domain.PurgePreviewSubtreeEntry{{ID: 24, EntryType: "FILE", Status: "ACTIVE", Revision: 1}}, nil
					}
					return []domain.PurgePreviewSubtreeEntry{{ID: 23, EntryType: "DIR", Status: "PURGING", Revision: 2}}, scenario.subtreeError
				},
			}
			progressService := NewPurgeProgressService(func(snapshotContext context.Context, read func(context.Context, PurgeProgressReader) error) error {
				if !scenario.skipRead {
					if err := read(snapshotContext, reader); err != nil {
						return err
					}
				}
				return scenario.finishError
			})
			result, err := progressService.Get(ctx, actor, 23)
			if result != nil || !errors.Is(err, scenario.want) || subtreeReads != scenario.wantSubtreeReads {
				t.Fatal("失败时应保留错误类别并停止交付结果", result, subtreeReads, err)
			}
		})
	}
}
