package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRecheckPurgePreview(t *testing.T) {
	masterKey := make([]byte, sha256.Size)
	signer, err := NewPurgePreviewSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewPurgePreviewVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	queryError := errors.New("子树读取故障")
	for _, scenario := range []struct {
		name         string
		wantError    error
		readsSubtree bool
	}{
		{name: "unchanged", readsSubtree: true},
		{name: "forbidden root before credential", wantError: domain.ErrRootEntryPurge},
		{name: "expired at cutoff", wantError: domain.ErrPurgePreviewChanged},
		{name: "credential for another user", wantError: domain.ErrInvalidPurgePreviewDigest},
		{name: "changed target revision", readsSubtree: true, wantError: domain.ErrPurgePreviewChanged},
		{name: "range too large", readsSubtree: true, wantError: domain.ErrPurgePreviewChanged},
		{name: "too many returned records", readsSubtree: true, wantError: domain.ErrPurgePreviewChanged},
		{name: "target inconsistent with subtree", readsSubtree: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "target missing from subtree read", readsSubtree: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "internal read error", readsSubtree: true, wantError: queryError},
		{name: "canceled during read", readsSubtree: true, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 根据第一次看到的目录项签发凭证，再准备确认时的读取结果。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			actor, target, paths := restoreEntryAncestorsFixture()
			entryID := target.ID
			entry := domain.PurgePreviewSubtreeEntry{
				ID: target.ID, EntryType: target.EntryType, Status: target.Status,
				Revision: target.Revision, FileSize: target.FileSize,
			}
			fingerprint, err := BuildPurgePreviewSubtreeFingerprint([]domain.PurgePreviewSubtreeEntry{entry})
			if err != nil {
				t.Fatal(err)
			}
			credentialUser := actor.UserID
			if scenario.name == "credential for another user" {
				credentialUser++
			}
			credential, err := signer.Sign(credentialUser, entryID, fingerprint, issuedAt)
			if err != nil {
				t.Fatal(err)
			}
			checkAt := credential.ExpiresAt.Add(-time.Nanosecond)
			switch scenario.name {
			case "forbidden root before credential":
				entryID = 11
			case "expired at cutoff":
				checkAt = credential.ExpiresAt
			case "changed target revision":
				target.Revision++
				entry.Revision = target.Revision
			}

			// 2. 核对读取使用同一事务上下文，并等待目标资格和凭证检查通过。
			subtreeReads, clockReads := 0, 0
			reader := purgePreviewReaderStub{paths, func(readContext context.Context, userID, scopeID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
				subtreeReads++
				if readContext != ctx || userID != actor.UserID || scopeID != entryID || clockReads != 1 {
					t.Fatal("子树读取顺序或事务参数错误")
				}
				switch scenario.name {
				case "range too large":
					return nil, fmt.Errorf("子树读取失败: %w", domain.ErrFileEntrySubtreeTooLarge)
				case "too many returned records":
					return make([]domain.PurgePreviewSubtreeEntry, domain.MaxFileEntrySubtreeNodes+1), nil
				case "target inconsistent with subtree":
					entry.Revision++
				case "target missing from subtree read":
					return nil, domain.ErrFileEntryNotFound
				case "internal read error":
					return nil, queryError
				case "canceled during read":
					cancel()
				}
				return []domain.PurgePreviewSubtreeEntry{entry}, nil
			}}
			result, err := RecheckPurgePreview(ctx, actor, entryID, credential.PreviewDigest, reader, verifier, func() time.Time {
				clockReads++
				if !slices.Equal(paths.entryIDs, []uint64{13, 12, 16, 11}) {
					t.Fatal("必须先检查目标和完整父链")
				}
				return checkAt
			})
			if (subtreeReads == 1) != scenario.readsSubtree || subtreeReads > 1 {
				t.Fatalf("复查读取子树 %d 次", subtreeReads)
			}
			if scenario.wantError != nil {
				if result != nil || !errors.Is(err, scenario.wantError) {
					t.Fatalf("拒绝结果为 %+v，错误 %v，期望 %v", result, err, scenario.wantError)
				}
				return
			}
			if err != nil || result == nil || result.ID != entryID || result.Revision != target.Revision || result.Status != "TRASHED" {
				t.Fatalf("复查未返回当前有效目标，结果 %+v，错误 %v", result, err)
			}
		})
	}
}
