package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type trashListReaderStub struct {
	*fileEntryPathReaderStub
	candidates []domain.TrashedFileEntry
	pagesRead  int
}

func (reader *trashListReaderStub) ListUserTrashEntryCandidates(_ context.Context, _ uint64, limit int, before *domain.TrashEntryPosition) (*domain.TrashEntryPage, error) {
	reader.pagesRead++
	page := &domain.TrashEntryPage{Entries: make([]domain.TrashedFileEntry, 0, limit)}
	for _, candidate := range reader.candidates {
		if before != nil && (candidate.DeletedAt.After(before.DeletedAt) ||
			(candidate.DeletedAt.Equal(before.DeletedAt) && candidate.ID >= before.ID)) {
			continue
		}
		if len(page.Entries) == limit {
			page.HasMore = true
			break
		}
		page.Entries = append(page.Entries, candidate)
	}
	return page, nil
}

func trashListFixture(count int, hidden func(int) bool) (domain.Actor, *trashListReaderStub) {
	actor, paths := fileEntryPathFixture()
	hiddenParent := *paths.entries[12]
	hiddenParent.ID, hiddenParent.Status = 14, "TRASHED"
	paths.entries[14] = &hiddenParent
	reader := &trashListReaderStub{fileEntryPathReaderStub: paths}
	for index := 0; index < count; index++ {
		entry := *paths.entries[13]
		entry.ID, entry.Status = uint64(2000-index), "TRASHED"
		if hidden(index) {
			entry.ParentID = &hiddenParent.ID
		}
		reader.candidates = append(reader.candidates, domain.TrashedFileEntry{FileEntry: entry, DeletedAt: entry.ModifiedAt})
	}
	return actor, reader
}

func TestTrashEntryListServiceResults(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		count     int
		hidden    func(int) bool
		wantIDs   []uint64
		nextID    uint64
		pagesRead int
	}{
		{"empty", 0, func(int) bool { return false }, nil, 0, 1},
		{"full last page", 2, func(int) bool { return false }, []uint64{2000, 1999}, 0, 1},
		{"unconsumed tail", 3, func(int) bool { return false }, []uint64{2000, 1999}, 1999, 1},
		{"mixed visibility", 5, func(index int) bool { return index%2 == 0 }, []uint64{1999, 1997}, 1997, 1},
		{"refill from next batch", 102, func(index int) bool { return index < 100 }, []uint64{1900, 1899}, 0, 2},
		{"empty at budget with continuation", 1001, func(index int) bool { return index < 1000 }, nil, 1001, 10},
		{"short page at budget", 1001, func(index int) bool { return index != 0 }, []uint64{2000}, 1001, 10},
		{"exact budget and exhausted", 1000, func(int) bool { return true }, nil, 0, 10},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 从同一快照读取候选和祖先，检查最后消费的位置。
			actor, reader := trashListFixture(testCase.count, testCase.hidden)
			snapshotCalls := 0
			list := NewTrashEntryListService(func(ctx context.Context, read func(context.Context, TrashEntryListReader) error) error {
				snapshotCalls++
				return read(ctx, reader)
			})
			page, err := list.List(t.Context(), actor, 2, nil)
			if err != nil || page == nil || page.Entries == nil || snapshotCalls != 1 || reader.pagesRead != testCase.pagesRead {
				t.Fatalf("列表页为 %+v，快照 %d 次、批次 %d 次，错误为 %v", page, snapshotCalls, reader.pagesRead, err)
			}
			ids := make([]uint64, 0, len(page.Entries))
			for _, item := range page.Entries {
				ids = append(ids, item.Entry.ID)
				path := item.OriginalPath.Entries
				if len(path) != 2 || path[0].ID != 11 || path[1].ID != 12 || *item.Entry.ParentID != 12 {
					t.Fatalf("可见项的原父路径为 %+v", path)
				}
			}
			if !slices.Equal(ids, testCase.wantIDs) || (page.NextPosition == nil) != (testCase.nextID == 0) ||
				(page.NextPosition != nil && page.NextPosition.ID != testCase.nextID) {
				t.Fatalf("结果为 %v，续读位置为 %+v，期望 %v 和 %d", ids, page.NextPosition, testCase.wantIDs, testCase.nextID)
			}
			// 2. 共享的祖先在一次快照内最多读取一次。
			if len(reader.userIDs) != len(reader.entryIDs)+1 {
				t.Fatal("同一快照重复查找了用户资料")
			}
			for index, entryID := range reader.entryIDs {
				if slices.Contains(reader.entryIDs[:index], entryID) {
					t.Fatalf("重复读取祖先 %d", entryID)
				}
			}
			if testCase.name == "empty at budget with continuation" {
				last, err := list.List(t.Context(), actor, 2, page.NextPosition)
				if err != nil || last == nil || len(last.Entries) != 1 || last.Entries[0].Entry.ID != 1000 || last.NextPosition != nil {
					t.Fatalf("空页续读结果为 %+v，错误为 %v", last, err)
				}
			}
		})
	}
}

func TestTrashEntryListServiceFailures(t *testing.T) {
	snapshotFailure := errors.New("只读快照结束失败")
	for _, testCase := range []struct {
		name          string
		repeatID      bool
		snapshotError error
		want          error
	}{
		{name: "snapshot fails after preparing result", snapshotError: snapshotFailure, want: snapshotFailure},
		{name: "candidate repeats with earlier time", repeatID: true, want: domain.ErrInvalidFileEntryData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := trashListFixture(2, func(int) bool { return false })
			if testCase.repeatID {
				reader.candidates[1].ID = reader.candidates[0].ID
				reader.candidates[1].DeletedAt = reader.candidates[0].DeletedAt.Add(-time.Microsecond)
			}
			list := NewTrashEntryListService(func(ctx context.Context, read func(context.Context, TrashEntryListReader) error) error {
				if err := read(ctx, reader); err != nil {
					return err
				}
				return testCase.snapshotError
			})
			if page, err := list.List(t.Context(), actor, 2, nil); page != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("错误时交付了 %+v，错误为 %v，期望 %v", page, err, testCase.want)
			}
		})
	}

	t.Run("invalid inputs avoid snapshot", func(t *testing.T) {
		actor, _ := trashListFixture(0, func(int) bool { return false })
		list := NewTrashEntryListService(func(context.Context, func(context.Context, TrashEntryListReader) error) error {
			t.Fatal("无效请求进入了快照")
			return nil
		})
		for _, pageSize := range []int{0, 101} {
			if page, err := list.List(t.Context(), actor, pageSize, nil); page != nil || err == nil {
				t.Fatal("无效页大小未返回错误")
			}
		}
		if page, err := list.List(t.Context(), actor, 1, &domain.TrashEntryPosition{}); page != nil || err == nil {
			t.Fatal("无效续读位置未返回错误")
		}
	})

	t.Run("disabled user with no candidates", func(t *testing.T) {
		actor, reader := trashListFixture(0, func(int) bool { return false })
		reader.profile.Status = "DISABLED"
		list := NewTrashEntryListService(func(ctx context.Context, read func(context.Context, TrashEntryListReader) error) error {
			return read(ctx, reader)
		})
		page, err := list.List(t.Context(), actor, 1, nil)
		if page != nil || !errors.Is(err, domain.ErrAccountDisabled) || reader.pagesRead != 0 {
			t.Fatalf("停用账号返回了 %+v，候选查询 %d 次，错误为 %v", page, reader.pagesRead, err)
		}
	})
}
