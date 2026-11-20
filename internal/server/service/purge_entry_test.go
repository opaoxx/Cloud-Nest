package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type purgeEntryTransactionStub struct {
	purgePreviewReaderStub
	begin func(context.Context, uint64, uint64) (*domain.FileEntry, error)
}

func (store purgeEntryTransactionStub) BeginUserEntryPurge(ctx context.Context, entryID, expectedRevision uint64) (*domain.FileEntry, error) {
	return store.begin(ctx, entryID, expectedRevision)
}

func TestPurgeEntryService(t *testing.T) {
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
	commitError := errors.New("受理事务提交失败")
	for _, scenario := range []struct {
		name                        string
		transactions, reads, writes int
		wantError                   error
	}{
		{"accepted", 1, 1, 1, nil},
		{"malformed credential", 0, 0, 0, domain.ErrInvalidPurgePreviewDigest},
		{"expired before revision check", 1, 0, 0, domain.ErrPurgePreviewChanged},
		{"changed range before revision check", 1, 1, 0, domain.ErrPurgePreviewChanged},
		{"wrong revision", 1, 1, 0, domain.ErrEntryRevisionConflict},
		{"exhausted revision", 1, 1, 0, domain.ErrEntryRevisionExhausted},
		{"wrong written target", 1, 1, 1, domain.ErrInvalidFileEntryData},
		{"canceled during write", 1, 1, 1, context.Canceled},
		{"commit failure", 1, 1, 1, commitError},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 为单项回收文件签发凭证，模拟受理时的当前版本。
			actor, target, paths := restoreEntryAncestorsFixture()
			if scenario.name == "exhausted revision" {
				target.Revision = math.MaxUint64
			}
			entry := domain.PurgePreviewSubtreeEntry{
				ID: target.ID, EntryType: target.EntryType, Status: target.Status,
				Revision: target.Revision, FileSize: target.FileSize,
			}
			fingerprint, err := BuildPurgePreviewSubtreeFingerprint([]domain.PurgePreviewSubtreeEntry{entry})
			if err != nil {
				t.Fatal(err)
			}
			credential, err := signer.Sign(actor.UserID, target.ID, fingerprint, issuedAt)
			if err != nil {
				t.Fatal(err)
			}
			expectedRevision, digest, now := target.Revision, credential.PreviewDigest, issuedAt.Add(time.Minute)
			switch scenario.name {
			case "malformed credential":
				digest = ""
			case "expired before revision check":
				now = credential.ExpiresAt
				expectedRevision++
			case "changed range before revision check":
				target.Revision++
				entry.Revision = target.Revision
			case "wrong revision":
				expectedRevision++
			}

			// 2. 核对读取和写入共享同一事务，范围一致后才允许写入。
			txContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			saved := *target
			parentID := *target.ParentID
			saved.ParentID = &parentID
			saved.ModifiedAt, saved.UpdatedAt = now, now
			saved.Status = "PURGING"
			transactions, reads, writes := 0, 0, 0
			reader := purgePreviewReaderStub{paths, func(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
				reads++
				if ctx != txContext || userID != actor.UserID || entryID != target.ID {
					t.Fatal("范围读取未使用当前事务或用户")
				}
				return []domain.PurgePreviewSubtreeEntry{entry}, nil
			}}
			store := purgeEntryTransactionStub{reader, func(ctx context.Context, entryID, revision uint64) (*domain.FileEntry, error) {
				writes++
				if ctx != txContext || entryID != target.ID || revision != expectedRevision || reads != 1 {
					t.Fatal("状态写入顺序或事务参数错误")
				}
				saved.Revision = revision + 1
				switch scenario.name {
				case "wrong written target":
					saved.ID++
				case "canceled during write":
					cancel()
				}
				return &saved, nil
			}}
			purge := NewPurgeEntryService(func(ctx context.Context, identity domain.Actor, action func(context.Context, PurgeEntryTransaction) error) error {
				transactions++
				if ctx != t.Context() || identity != actor {
					t.Fatal("事务入口未收到原请求或认证身份")
				}
				if err := action(txContext, store); err != nil {
					return err
				}
				if scenario.name == "commit failure" {
					return commitError
				}
				return nil
			}, verifier, func() time.Time { return now })

			result, err := purge.Begin(t.Context(), actor, target.ID, expectedRevision, digest)
			if transactions != scenario.transactions || reads != scenario.reads || writes != scenario.writes ||
				!errors.Is(err, scenario.wantError) || (result == nil) != (scenario.wantError != nil) {
				t.Fatalf("调用次数为 %d/%d/%d，结果 %+v，错误 %v", transactions, reads, writes, result, err)
			}
			if scenario.wantError == domain.ErrEntryRevisionConflict {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != target.Revision {
					t.Fatal("未返回用户事务中读取的版本", err)
				}
			}
			if result != nil && (result.ID != target.ID || result.Revision != expectedRevision+1 || result.Status != "PURGING" ||
				result.ParentID == saved.ParentID || *result.ParentID != *saved.ParentID) {
				t.Fatal("提交后应获得独立、完整的受理结果", result)
			}
		})
	}
}
