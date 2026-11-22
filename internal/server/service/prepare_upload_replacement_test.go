package service

import (
	"context"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type uploadReplacementLockerFunc func(context.Context, uint64, uint64) (*domain.FileEntry, error)

func (lock uploadReplacementLockerFunc) LockUploadReplacementTarget(ctx context.Context, entryID, revision uint64) (*domain.FileEntry, error) {
	return lock(ctx, entryID, revision)
}

func TestPrepareUploadReplacementPreconditions(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	declaration := UploadReplacementDeclaration{EntryID: 31, ExpectedRevision: 6}
	for _, fixture := range []struct {
		name        string
		actor       domain.Actor
		declaration UploadReplacementDeclaration
	}{
		{"missing session", domain.Actor{UserID: 7, DeviceID: 11}, declaration},
		{"zero target", actor, UploadReplacementDeclaration{ExpectedRevision: 6}},
		{"zero revision", actor, UploadReplacementDeclaration{EntryID: 31}},
		{"unsafe size", actor, UploadReplacementDeclaration{EntryID: 31, ExpectedRevision: 6, SizeBytes: 1 << 53}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			locker := uploadReplacementLockerFunc(func(context.Context, uint64, uint64) (*domain.FileEntry, error) {
				t.Fatal("无效前置条件仍进入目标锁读")
				return nil, nil
			})
			prepared, err := PrepareUploadReplacement(t.Context(), fixture.actor, fixture.declaration, domain.UploadPlanLimits{}, locker)
			if err == nil || prepared != nil {
				t.Fatalf("无效声明应返回错误和空结果，得到 %+v，错误 %v", prepared, err)
			}
		})
	}
}

func TestPrepareUploadReplacementReadBoundary(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	declaration := UploadReplacementDeclaration{EntryID: 31, ExpectedRevision: 6}
	limits := domain.UploadPlanLimits{MaxFileSizeBytes: 1, MaxObjectSizeBytes: 1, MaxPartSizeBytes: domain.MaxSingleUploadSizeBytes}
	parentID := uint64(33)
	target := domain.FileEntry{ID: 31, UserID: 7, ParentID: &parentID, Name: "笔记.MD", EntryType: "FILE", Status: "ACTIVE", Revision: 6}
	for _, fixture := range []struct {
		name   string
		change func(*domain.FileEntry)
		cancel bool
		want   error
	}{
		{name: "wrong user from locker", change: func(entry *domain.FileEntry) { entry.UserID++ }, want: domain.ErrInvalidFileEntryData},
		{name: "wrong revision from locker", change: func(entry *domain.FileEntry) { entry.Revision++ }, want: domain.ErrInvalidFileEntryData},
		{name: "missing parent from locker", change: func(entry *domain.FileEntry) { entry.ParentID = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "cancelled during read", cancel: true, want: context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			locker := uploadReplacementLockerFunc(func(_ context.Context, entryID, revision uint64) (*domain.FileEntry, error) {
				if entryID != declaration.EntryID || revision != declaration.ExpectedRevision {
					t.Fatal("锁读未使用原请求目标与版本")
				}
				entry := target
				if fixture.change != nil {
					fixture.change(&entry)
				}
				if fixture.cancel {
					cancel()
				}
				return &entry, nil
			})
			prepared, err := PrepareUploadReplacement(ctx, actor, declaration, limits, locker)
			if prepared != nil || !errors.Is(err, fixture.want) {
				t.Fatalf("锁读边界应返回 %v 和空结果，得到 %+v，错误 %v", fixture.want, prepared, err)
			}
		})
	}
}
