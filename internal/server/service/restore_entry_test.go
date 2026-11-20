package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type restoreEntryTransactionStub struct {
	*fileEntryPathReaderStub
	calls      []string
	stats      domain.EntrySubtreeStats
	statsError error
	nameTaken  bool
	nameError  error
	writeError error
	onWrite    func(*domain.FileEntry)
	saved      *domain.FileEntry
}

func (store *restoreEntryTransactionStub) FindUserEntrySubtreeStats(_ context.Context, userID, entryID uint64) (*domain.EntrySubtreeStats, error) {
	store.calls = append(store.calls, fmt.Sprintf("stats:%d/%d", userID, entryID))
	return &store.stats, store.statsError
}

func (store *restoreEntryTransactionStub) FindUserActiveFileEntryByName(_ context.Context, userID, parentID uint64, key []byte) (*domain.FileEntry, error) {
	store.calls = append(store.calls, fmt.Sprintf("name:%d/%d/%s", userID, parentID, key))
	if store.nameError != nil {
		return nil, store.nameError
	}
	if store.nameTaken && string(key) == "学习笔记.md" {
		return &domain.FileEntry{ID: 22, UserID: userID, ParentID: &parentID, Status: "ACTIVE"}, nil
	}
	return nil, domain.ErrFileEntryNotFound
}

func (store *restoreEntryTransactionStub) RestoreUserFileEntry(_ context.Context, entryID, revision, parentID uint64, name string, key []byte) (*domain.FileEntry, error) {
	store.calls = append(store.calls, fmt.Sprintf("write:%d/%d/%d/%s", entryID, revision, parentID, key))
	if store.writeError != nil {
		return nil, store.writeError
	}
	saved := *store.entries[entryID]
	saved.Status, saved.Revision, saved.ParentID, saved.Name = "ACTIVE", revision+1, &parentID, name
	saved.ModifiedAt = saved.CreatedAt.Add(time.Second)
	saved.UpdatedAt = saved.ModifiedAt
	if store.onWrite != nil {
		store.onWrite(&saved)
	}
	store.saved = &saved
	return store.saved, nil
}

func restoreEntryServiceFixture() (domain.Actor, *restoreEntryTransactionStub) {
	actor, reader := fileEntryPathFixture()
	reader.entries[13].Status = "TRASHED"
	store := &restoreEntryTransactionStub{
		fileEntryPathReaderStub: reader,
		stats:                   domain.EntrySubtreeStats{UserID: actor.UserID, EntryID: 13, NodeCount: 1},
	}
	reader.afterEntry = func(entryID uint64) {
		store.calls = append(store.calls, fmt.Sprintf("read:%d", entryID))
	}
	reader.afterProfile = func() { store.calls = append(store.calls, "profile") }
	return actor, store
}

func TestRestoreEntryServiceOrderAndResult(t *testing.T) {
	requestContext := t.Context()
	txContext, cancel := context.WithCancel(requestContext)
	defer cancel()
	actor, store := restoreEntryServiceFixture()
	store.entries[12].Status = "TRASHED"
	store.nameTaken = true
	core := NewRestoreEntryService(func(ctx context.Context, identity domain.Actor, action func(context.Context, RestoreEntryTransaction) error) error {
		if ctx != requestContext || identity != actor {
			t.Fatal("事务未收到原始上下文和身份")
		}
		store.calls = append(store.calls, "begin")
		if err := action(txContext, store); err != nil {
			return err
		}
		store.calls = append(store.calls, "commit")
		*store.saved.ParentID = 99
		return nil
	})
	result, err := core.Restore(requestContext, actor, 13, 3)
	if err != nil || result == nil || result.Name != "学习笔记 (1).md" || result.ParentID == nil ||
		*result.ParentID != 11 || result.Revision != 4 || result.Status != "ACTIVE" || result == store.saved {
		t.Fatalf("恢复结果为 %+v，错误为 %v", result, err)
	}
	want := []string{
		"begin", "read:13", "profile", "profile", "read:12", "read:11", "stats:7/13",
		"name:7/11/学习笔记.md", "name:7/11/学习笔记 (1).md", "write:13/3/11/学习笔记 (1).md", "commit",
	}
	if !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("调用顺序为 %v，期望 %v", store.calls, want)
	}
}

func TestRestoreEntryServiceFailureStopsFlow(t *testing.T) {
	queryFailure := errors.New("fixture query")
	writeFailure := errors.New("fixture write")
	commitFailure := errors.New("fixture commit")
	for _, scenario := range []struct {
		name     string
		entryID  uint64
		revision uint64
		change   func(*restoreEntryTransactionStub)
		commit   error
		lastCall string
		want     error
	}{
		{name: "real root", entryID: 11, revision: 1, lastCall: "profile", want: domain.ErrRootEntryRestore},
		{name: "purging ancestor before version", entryID: 13, revision: 2, change: func(store *restoreEntryTransactionStub) { store.entries[12].Status = "PURGING" }, lastCall: "read:11", want: domain.ErrFileEntryNotFound},
		{name: "version before stats", entryID: 13, revision: 2, lastCall: "read:11", want: domain.ErrEntryRevisionConflict},
		{name: "version exhausted", entryID: 13, revision: math.MaxUint64, change: func(store *restoreEntryTransactionStub) { store.entries[13].Revision = math.MaxUint64 }, lastCall: "read:11", want: domain.ErrEntryRevisionExhausted},
		{name: "stats failure", entryID: 13, revision: 3, change: func(store *restoreEntryTransactionStub) { store.statsError = queryFailure }, lastCall: "stats:7/13", want: queryFailure},
		{name: "scope mismatch", entryID: 13, revision: 3, change: func(store *restoreEntryTransactionStub) { store.stats.UserID++ }, lastCall: "stats:7/13", want: domain.ErrInvalidFileEntryData},
		{name: "lookup failure", entryID: 13, revision: 3, change: func(store *restoreEntryTransactionStub) { store.nameError = queryFailure }, lastCall: "name:7/12/学习笔记.md", want: queryFailure},
		{name: "write failure", entryID: 13, revision: 3, change: func(store *restoreEntryTransactionStub) { store.writeError = writeFailure }, lastCall: "write:13/3/12/学习笔记.md", want: writeFailure},
		{name: "unexpected write result", entryID: 13, revision: 3, change: func(store *restoreEntryTransactionStub) {
			store.onWrite = func(entry *domain.FileEntry) { entry.Revision++ }
		}, lastCall: "write:13/3/12/学习笔记.md", want: domain.ErrInvalidFileEntryData},
		{name: "commit failure", entryID: 13, revision: 3, commit: commitFailure, lastCall: "write:13/3/12/学习笔记.md", want: commitFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, store := restoreEntryServiceFixture()
			if scenario.change != nil {
				scenario.change(store)
			}
			core := NewRestoreEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, RestoreEntryTransaction) error) error {
				if err := action(ctx, store); err != nil {
					return err
				}
				return scenario.commit
			})
			result, err := core.Restore(t.Context(), actor, scenario.entryID, scenario.revision)
			if result != nil || !errors.Is(err, scenario.want) || len(store.calls) == 0 || store.calls[len(store.calls)-1] != scenario.lastCall {
				t.Fatalf("错误停止位置或类别有误，结果为 %+v，调用为 %v，错误为 %v", result, store.calls, err)
			}
		})
	}
}

func TestRestoreEntryServiceCancellation(t *testing.T) {
	actor, store := restoreEntryServiceFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store.onWrite = func(*domain.FileEntry) { cancel() }
	core := NewRestoreEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, RestoreEntryTransaction) error) error {
		return action(ctx, store)
	})
	result, err := core.Restore(ctx, actor, 13, 3)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("写入期间取消后仍返回恢复结果，结果 %+v，错误 %v", result, err)
	}
}
