package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type visibilityReaderStub struct {
	*fileEntryPathReaderStub
	batches    [][]uint64
	batchError error
	change     func(map[uint64]domain.FileEntry)
	afterBatch func()
	nilMap     bool
}

func (reader *visibilityReaderStub) FindUserFileEntries(ctx context.Context, userID uint64, ids []uint64) (map[uint64]domain.FileEntry, error) {
	reader.batches = append(reader.batches, slices.Clone(ids))
	reader.contexts = append(reader.contexts, ctx)
	reader.userIDs = append(reader.userIDs, userID)
	entries := make(map[uint64]domain.FileEntry)
	for _, entryID := range ids {
		if entry := reader.entries[entryID]; entry != nil {
			entries[entryID] = *entry
		}
	}
	if reader.change != nil {
		reader.change(entries)
	}
	if reader.afterBatch != nil {
		reader.afterBatch()
	}
	if reader.nilMap {
		entries = nil
	}
	return entries, reader.batchError
}

func visibilityFixture() (domain.Actor, *visibilityReaderStub, []domain.FileEntry) {
	actor, reader := fileEntryPathFixture()
	return actor, &visibilityReaderStub{fileEntryPathReaderStub: reader}, []domain.FileEntry{*reader.entries[13]}
}

func TestFilterVisibleFileEntriesResults(t *testing.T) {
	for _, mode := range []string{"file", "empty", "file and parent", "siblings", "mixed hidden and visible", "hidden target TRASHED", "hidden target PURGING", "hidden target PURGED", "hidden parent TRASHED", "hidden parent PURGING", "hidden parent PURGED", "maximum candidates", "maximum distinct parents"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备同一快照内的候选及祖先，核对筛选和分层批量读取。
			actor, reader, candidates := visibilityFixture()
			wantIDs, wantBatches := []uint64{13}, 2
			switch mode {
			case "empty":
				candidates, wantIDs, wantBatches = nil, nil, 0
			case "file and parent":
				candidates = append(candidates, *reader.entries[12])
				wantIDs, wantBatches = []uint64{13, 12}, 1
			case "siblings":
				sibling := candidates[0]
				sibling.ID = 14
				candidates = append([]domain.FileEntry{sibling}, candidates...)
				wantIDs = []uint64{14, 13}
			case "mixed hidden and visible":
				hiddenParent := *reader.entries[12]
				hiddenParent.ID, hiddenParent.Status = 21, "TRASHED"
				reader.entries[21] = &hiddenParent
				hidden := candidates[0]
				hidden.ID, hidden.ParentID = 14, copyOptionalValue(&hiddenParent.ID)
				candidates = append([]domain.FileEntry{hidden}, candidates...)
			case "hidden target TRASHED", "hidden target PURGING", "hidden target PURGED":
				candidates[0].Status = mode[len("hidden target "):]
				wantIDs, wantBatches = nil, 0
			case "hidden parent TRASHED", "hidden parent PURGING", "hidden parent PURGED":
				reader.entries[12].Status = mode[len("hidden parent "):]
				wantIDs, wantBatches = nil, 1
			case "maximum candidates":
				file := candidates[0]
				candidates, wantIDs = nil, nil
				for index := range domain.MaxGlobalEntryCandidates {
					file.ID = uint64(100 + index)
					candidates = append(candidates, file)
					wantIDs = append(wantIDs, file.ID)
				}
			case "maximum distinct parents":
				file := candidates[0]
				candidates, wantIDs = nil, nil
				for index := range domain.MaxGlobalEntryCandidates {
					parent := *reader.entries[12]
					parent.ID = uint64(1000 + index)
					reader.entries[parent.ID] = &parent
					file.ID, file.ParentID = uint64(10000+index), copyOptionalValue(&parent.ID)
					candidates = append(candidates, file)
					wantIDs = append(wantIDs, file.ID)
				}
			}
			before := copyFileEntryRecords(candidates)
			sources := make(map[uint64]domain.FileEntry, len(candidates))
			for _, entry := range candidates {
				sources[entry.ID] = entry
			}
			ctx := context.WithValue(context.Background(), struct{}{}, "snapshot")
			result, err := FilterVisibleFileEntries(ctx, actor, candidates, reader)
			if err != nil || result == nil || len(result) != len(wantIDs) || len(reader.batches) != wantBatches || !reflect.DeepEqual(before, copyFileEntryRecords(candidates)) {
				t.Fatal("可见项、批次数或输入保护错误", err)
			}
			for index, entryID := range wantIDs {
				if !reflect.DeepEqual(result[index], sources[entryID]) || result[index].ParentID == nil || result[index].ParentID == sources[entryID].ParentID {
					t.Fatal("筛选顺序或父目录副本错误")
				}
			}
			seen := make(map[uint64]bool)
			for _, batch := range reader.batches {
				if len(batch) > domain.MaxGlobalEntryCandidates {
					t.Fatal("祖先批次超过读取上限")
				}
				for _, entryID := range batch {
					if seen[entryID] {
						t.Fatal("共享或缺失祖先被重复读取")
					}
					seen[entryID] = true
				}
			}
			if len(reader.entryIDs) != 0 || len(reader.contexts) != len(reader.batches)+1 {
				t.Fatal("未复用批量读取或用户资料被重复查询")
			}
			for _, userID := range reader.userIDs {
				if userID != actor.UserID {
					t.Fatal("祖先查询用户范围错误")
				}
			}
			for _, received := range reader.contexts {
				if received != ctx {
					t.Fatal("读取能力未使用当前快照上下文")
				}
			}
		})
	}
}

func TestFilterVisibleFileEntriesFailures(t *testing.T) {
	readFailure := errors.New("fixture batch failure")
	for _, fixture := range []struct {
		mode string
		want error
	}{
		{"missing parent", domain.ErrInvalidFileEntryData},
		{"missing root", domain.ErrInvalidFileEntryData},
		{"parent cycle", domain.ErrInvalidFileEntryData},
		{"file ancestor", domain.ErrInvalidFileEntryData},
		{"directory size", domain.ErrInvalidFileEntryData},
		{"unknown state", domain.ErrInvalidFileEntryData},
		{"empty parent name", domain.ErrInvalidFileEntryData},
		{"wrong root", domain.ErrInvalidFileEntryData},
		{"root name", domain.ErrInvalidFileEntryData},
		{"root has parent", domain.ErrInvalidFileEntryData},
		{"foreign ancestor", domain.ErrInvalidFileEntryData},
		{"wrong map identifier", domain.ErrInvalidFileEntryData},
		{"unrequested map entry", domain.ErrInvalidFileEntryData},
		{"nil map", domain.ErrInvalidFileEntryData},
		{"zero ancestor revision", domain.ErrInvalidFileEntryData},
		{"zero ancestor parent", domain.ErrInvalidFileEntryData},
		{"unknown target state", domain.ErrInvalidFileEntryData},
		{"unknown target type", domain.ErrInvalidFileEntryData},
		{"disabled account", domain.ErrAccountDisabled},
		{"disabled empty batch", domain.ErrAccountDisabled},
		{"missing account", domain.ErrAccountNotFound},
		{"invalid profile", domain.ErrInvalidAccountData},
		{"batch failure", readFailure},
		{"later malformed candidate", domain.ErrInvalidFileEntryData},
	} {
		t.Run(fixture.mode, func(t *testing.T) {
			// 1. 复用已有路径错误规则，异常时整体丢弃结果。
			actor, reader, candidates := visibilityFixture()
			switch fixture.mode {
			case "missing parent":
				delete(reader.entries, 12)
			case "missing root":
				delete(reader.entries, 11)
			case "parent cycle":
				reader.entries[12].ParentID = copyOptionalValue(&reader.entries[12].ID)
			case "file ancestor":
				reader.entries[12].EntryType, reader.entries[12].FileSize = "FILE", 2048
			case "directory size":
				reader.entries[12].FileSize = 1
			case "unknown state":
				reader.entries[12].Status = "UNKNOWN"
			case "empty parent name":
				reader.entries[12].Name = ""
			case "wrong root":
				reader.profile.RootEntryID = 99
			case "root name":
				reader.entries[11].Name = "wrong-root"
			case "root has parent":
				reader.entries[11].ParentID = copyOptionalValue(&reader.entries[12].ID)
			case "foreign ancestor":
				reader.entries[12].UserID = 8
			case "wrong map identifier":
				reader.change = func(entries map[uint64]domain.FileEntry) {
					entry := entries[12]
					entry.ID = 99
					entries[12] = entry
				}
			case "unrequested map entry":
				reader.change = func(entries map[uint64]domain.FileEntry) { entries[99] = *reader.entries[11] }
			case "nil map":
				reader.nilMap = true
			case "zero ancestor revision":
				reader.entries[12].Revision = 0
			case "zero ancestor parent":
				reader.entries[12].ParentID = new(uint64)
			case "unknown target state":
				candidates[0].Status = "UNKNOWN"
			case "unknown target type":
				candidates[0].EntryType = "LINK"
			case "disabled account":
				reader.profile.Status = "DISABLED"
			case "disabled empty batch":
				reader.profile.Status, candidates = "DISABLED", nil
			case "missing account":
				reader.profileError = domain.ErrAccountNotFound
			case "invalid profile":
				reader.profile.RootEntryID = 0
			case "batch failure":
				reader.batchError = readFailure
			case "later malformed candidate":
				second := candidates[0]
				second.ID, second.EntryType = 14, "LINK"
				candidates = append(candidates, second)
			}
			result, err := FilterVisibleFileEntries(context.Background(), actor, candidates, reader)
			if result != nil || !errors.Is(err, fixture.want) {
				t.Fatal("异常父链或读取错误处理不符", err)
			}
		})
	}
}

func TestFilterVisibleFileEntriesBoundsAndCopies(t *testing.T) {
	for _, depth := range []int{32, 33} {
		t.Run(fmt.Sprintf("depth %d", depth), func(t *testing.T) {
			actor, reader, candidates := visibilityFixture()
			parentID := uint64(11)
			for level := 1; level < depth; level++ {
				directory := *reader.entries[12]
				directory.ID, directory.ParentID = uint64(100+level), copyOptionalValue(&parentID)
				reader.entries[directory.ID] = &directory
				parentID = directory.ID
			}
			candidates[0].ParentID = &parentID
			result, err := FilterVisibleFileEntries(context.Background(), actor, candidates, reader)
			if depth == 32 && (err != nil || len(result) != 1) {
				t.Fatal("合法三十二层被拒绝", err)
			}
			if depth == 33 && (result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData)) {
				t.Fatal("超过三十二层未拒绝", err)
			}
			if len(reader.batches) != 32 {
				t.Fatal("祖先读取轮数未受最大深度约束")
			}
		})
	}
	t.Run("cached records are independent", func(t *testing.T) {
		actor, reader, candidates := visibilityFixture()
		reader.afterBatch = func() {
			if len(reader.batches) == 1 {
				reader.profile.RootEntryID = 999
				candidates[0].Name = "调用方后续修改"
				*candidates[0].ParentID = 999
			} else {
				*reader.entries[12].ParentID = 999
			}
		}
		result, err := FilterVisibleFileEntries(context.Background(), actor, candidates, reader)
		if err != nil || len(result) != 1 || result[0].Name != "学习笔记.md" || *result[0].ParentID != 12 {
			t.Fatal("源记录后续修改影响缓存结果", err)
		}
	})
}

func TestFilterVisibleFileEntriesInputsAndCancellation(t *testing.T) {
	for _, mode := range []string{"nil context", "nil reader", "zero user", "zero session", "zero device", "oversized", "zero candidate", "foreign candidate", "zero revision", "root candidate", "zero parent", "duplicate candidate", "cancelled", "expired", "after profile", "after batch", "after failed batch"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, candidates := visibilityFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var inputContext context.Context = ctx
			var inputReader FileEntryVisibilityReader = reader
			var want error
			switch mode {
			case "nil context":
				inputContext = nil
			case "nil reader":
				inputReader = nil
			case "zero user":
				actor.UserID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero device":
				actor.DeviceID = 0
			case "oversized":
				candidates = make([]domain.FileEntry, domain.MaxGlobalEntryCandidates+1)
			case "zero candidate":
				candidates[0].ID = 0
			case "foreign candidate":
				candidates[0].UserID = 8
			case "zero revision":
				candidates[0].Revision = 0
			case "root candidate":
				candidates[0] = *reader.entries[11]
			case "zero parent":
				candidates[0].ParentID = new(uint64)
			case "duplicate candidate":
				candidates = append(candidates, candidates[0])
			case "cancelled":
				cancel()
				want = context.Canceled
			case "expired":
				var release context.CancelFunc
				inputContext, release = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				want = context.DeadlineExceeded
			case "after profile":
				reader.afterProfile, want = cancel, context.Canceled
			case "after batch":
				reader.afterBatch, want = cancel, context.Canceled
			case "after failed batch":
				reader.afterBatch, reader.batchError, want = cancel, domain.ErrInvalidFileEntryData, context.Canceled
			}
			result, err := FilterVisibleFileEntries(inputContext, actor, candidates, inputReader)
			if result != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("输入或取消边界错误", err)
			}
			if want == nil && len(reader.contexts) != 0 {
				t.Fatal("非法输入调用了读取能力")
			}
		})
	}
}
