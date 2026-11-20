package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type purgePreviewReaderStub struct {
	*fileEntryPathReaderStub
	readSubtree func(context.Context, uint64, uint64) ([]domain.PurgePreviewSubtreeEntry, error)
}

func (reader purgePreviewReaderStub) ListUserPurgePreviewSubtree(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
	return reader.readSubtree(ctx, userID, entryID)
}

func TestPurgePreviewService(t *testing.T) {
	signer, err := NewPurgePreviewSigner(make([]byte, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	finishError := errors.New("结束快照失败")
	issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)

	for _, scenario := range []struct {
		name             string
		targetID         uint64
		changeSubtree    func(domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry
		failOnFinish     bool
		cancelOnRead     bool
		wantSubtreeReads int
		wantError        error
	}{
		{name: "file preview", targetID: 13, wantSubtreeReads: 1},
		{name: "root rejected before subtree", targetID: 11, wantError: domain.ErrRootEntryPurge},
		{name: "zero target rejected before reads", wantError: nil},
		{"missing target from subtree", 13, func(root domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			root.ID++
			return []domain.PurgePreviewSubtreeEntry{root}
		}, false, false, 1, domain.ErrInvalidFileEntryData},
		{"mismatched revision", 13, func(root domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			root.Revision++
			return []domain.PurgePreviewSubtreeEntry{root}
		}, false, false, 1, domain.ErrInvalidFileEntryData},
		{name: "snapshot finish failure", targetID: 13, failOnFinish: true, wantSubtreeReads: 1, wantError: finishError},
		{name: "cancelled during read", targetID: 13, cancelOnRead: true, wantSubtreeReads: 1, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, target, paths := restoreEntryAncestorsFixture()
			snapshotContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			root := domain.PurgePreviewSubtreeEntry{
				ID: target.ID, Revision: target.Revision, EntryType: target.EntryType, Status: target.Status, FileSize: target.FileSize,
			}
			subtreeReads := 0
			reader := purgePreviewReaderStub{paths, func(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
				subtreeReads++
				if ctx != snapshotContext || userID != actor.UserID || entryID != target.ID ||
					!slices.Equal(paths.entryIDs, []uint64{13, 12, 16, 11}) {
					t.Fatal("子树读取须使用同一快照和用户，并晚于目标与祖先检查")
				}
				if scenario.cancelOnRead {
					cancel()
				}
				if scenario.changeSubtree != nil {
					return scenario.changeSubtree(root), nil
				}
				return []domain.PurgePreviewSubtreeEntry{root}, nil
			}}
			snapshots, clockReads := 0, 0
			finished := false
			previewService := NewPurgePreviewService(func(ctx context.Context, action func(context.Context, PurgePreviewReader) error) error {
				snapshots++
				if err := action(snapshotContext, reader); err != nil {
					return err
				}
				if scenario.failOnFinish {
					return finishError
				}
				finished = true
				return nil
			}, signer, func() time.Time {
				clockReads++
				if !finished {
					t.Fatal("快照结束后才应读取签发时间")
				}
				return issuedAt
			})

			result, err := previewService.Preview(t.Context(), actor, scenario.targetID)
			if subtreeReads != scenario.wantSubtreeReads {
				t.Fatalf("子树读取 %d 次，预期 %d 次", subtreeReads, scenario.wantSubtreeReads)
			}
			if scenario.targetID == 0 {
				if snapshots != 0 || clockReads != 0 || result != nil || err == nil {
					t.Fatal("零目标应在打开快照前拒绝", result, err)
				}
				return
			}
			if snapshots != 1 {
				t.Fatalf("预检打开了 %d 份快照", snapshots)
			}
			if scenario.wantError != nil {
				if result != nil || !errors.Is(err, scenario.wantError) || clockReads != 0 {
					t.Fatalf("错误时应停止且无凭证，结果 %+v，错误 %v，期望 %v", result, err, scenario.wantError)
				}
				return
			}
			if err != nil || result == nil || clockReads != 1 || result.EntryID != target.ID || result.Revision != target.Revision ||
				result.Summary != (domain.PurgePreviewSummary{FileCount: 1, LogicalBytes: target.FileSize}) ||
				result.Credential.PreviewDigest == "" || !result.Credential.ExpiresAt.Equal(issuedAt.Add(5*time.Minute)) {
				t.Fatalf("预检结果或凭证不符 %+v，错误 %v", result, err)
			}
		})
	}
}
