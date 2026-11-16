package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func entryMoveLimitsFixture(parentDepth int, entryType string, count, relativeDepth uint64) (*domain.EntryMovePaths, *domain.EntrySubtreeStats) {
	_, reader := fileEntryPathFixture()
	root, directory, source := *reader.entries[11], *reader.entries[12], *reader.entries[13]
	source.EntryType = entryType
	if entryType == "DIR" {
		source.FileSize = 0
	}
	targetEntries := []domain.FileEntry{root}
	parentID := root.ID
	for depth := 1; depth <= parentDepth; depth++ {
		entry := directory
		entry.ID, entry.Name = uint64(100+depth), fmt.Sprintf("目的地%d", depth)
		entry.ParentID = copyOptionalValue(&parentID)
		targetEntries = append(targetEntries, entry)
		parentID = entry.ID
	}
	return &domain.EntryMovePaths{
		Source:       domain.FileEntryPath{Entries: []domain.FileEntry{root, directory, source}},
		TargetParent: domain.FileEntryPath{Entries: targetEntries},
	}, &domain.EntrySubtreeStats{UserID: source.UserID, EntryID: source.ID, NodeCount: count, MaxRelativeDepth: relativeDepth}
}

func TestValidateEntryMoveLimitsBounds(t *testing.T) {
	for _, testCase := range []struct {
		name, entryType      string
		parentDepth          int
		count, relativeDepth uint64
		want                 error
	}{
		{"file to root", "FILE", 0, 1, 0, nil},
		{"empty folder to root", "DIR", 0, 1, 0, nil},
		{"file final depth 32", "FILE", 31, 1, 0, nil},
		{"file final depth 33", "FILE", 32, 1, 0, domain.ErrFileEntryDepthExceeded},
		{"folder final depth 32", "DIR", 30, 2, 1, nil},
		{"folder final depth 33", "DIR", 31, 2, 1, domain.ErrFileEntryDepthExceeded},
		{"relative 31 to root", "DIR", 0, 32, 31, nil},
		{"relative 32 to root", "DIR", 0, 33, 32, domain.ErrFileEntryDepthExceeded},
		{"below count limit", "DIR", 1, 9999, 1, nil},
		{"at count limit", "DIR", 1, 10000, 1, nil},
		{"above count limit", "DIR", 1, 10001, 1, domain.ErrFileEntrySubtreeTooLarge},
		{"size before depth", "DIR", 31, 10001, 1, domain.ErrFileEntrySubtreeTooLarge},
		{"maximum count", "DIR", 0, ^uint64(0), 1, domain.ErrFileEntrySubtreeTooLarge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			paths, stats := entryMoveLimitsFixture(testCase.parentDepth, testCase.entryType, testCase.count, testCase.relativeDepth)
			err := ValidateEntryMoveLimits(paths, stats)
			if (testCase.want == nil && err != nil) || (testCase.want != nil && !errors.Is(err, testCase.want)) {
				t.Fatal("移动限制边界错误", err)
			}
			if testCase.want != nil && err.Error() != testCase.want.Error() {
				t.Fatal("限制错误提示不稳定")
			}
		})
	}
	if domain.MaxFileEntrySubtreeNodes != 10000 {
		t.Fatal("子树上限与方案不一致")
	}
}

func TestValidateEntryMoveLimitsInvalidInputs(t *testing.T) {
	for _, mode := range []string{
		"nil paths", "nil stats", "empty source", "root source", "empty destination", "long source", "long destination",
		"zero source", "zero owner", "nil source parent", "zero source parent", "self source parent", "zero destination", "foreign destination", "file destination",
		"zero root", "different roots", "foreign source root", "foreign destination root", "foreign stats", "wrong stats source",
		"zero count", "relative depth beyond bound", "impossible depth count", "many zero depth nodes", "file with children", "unknown source type",
	} {
		t.Run(mode, func(t *testing.T) {
			paths, stats := entryMoveLimitsFixture(1, "DIR", 3, 1)
			source := &paths.Source.Entries[len(paths.Source.Entries)-1]
			target := &paths.TargetParent.Entries[len(paths.TargetParent.Entries)-1]
			switch mode {
			case "nil paths":
				paths = nil
			case "nil stats":
				stats = nil
			case "empty source":
				paths.Source.Entries = nil
			case "root source":
				paths.Source.Entries = paths.Source.Entries[:1]
			case "empty destination":
				paths.TargetParent.Entries = nil
			case "long source":
				paths.Source.Entries = make([]domain.FileEntry, domain.MaxFileEntryDepth+2)
			case "long destination":
				paths.TargetParent.Entries = make([]domain.FileEntry, domain.MaxFileEntryDepth+2)
			case "zero source":
				source.ID = 0
			case "zero owner":
				source.UserID = 0
			case "nil source parent":
				source.ParentID = nil
			case "zero source parent":
				parent := uint64(0)
				source.ParentID = &parent
			case "self source parent":
				source.ParentID = &source.ID
			case "zero destination":
				target.ID = 0
			case "foreign destination":
				target.UserID++
			case "file destination":
				target.EntryType = "FILE"
			case "zero root":
				paths.Source.Entries[0].ID = 0
			case "different roots":
				paths.TargetParent.Entries[0].ID = 91
			case "foreign source root":
				paths.Source.Entries[0].UserID++
			case "foreign destination root":
				paths.TargetParent.Entries[0].UserID++
			case "foreign stats":
				stats.UserID++
			case "wrong stats source":
				stats.EntryID++
			case "zero count":
				stats.NodeCount = 0
			case "relative depth beyond bound":
				stats.NodeCount, stats.MaxRelativeDepth = ^uint64(0), ^uint64(0)
			case "impossible depth count":
				stats.NodeCount, stats.MaxRelativeDepth = 2, 2
			case "many zero depth nodes":
				stats.NodeCount, stats.MaxRelativeDepth = 3, 0
			case "file with children":
				source.EntryType = "FILE"
			case "unknown source type":
				source.EntryType = "UNKNOWN"
			}
			if err := ValidateEntryMoveLimits(paths, stats); !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("无效内部输入未拒绝", err)
			}
		})
	}
}

func TestValidateEntryMoveLimitsScopeCopiesAndConcurrent(t *testing.T) {
	paths, stats := entryMoveLimitsFixture(0, "DIR", 10000, 31)
	maximum := ^uint64(0)
	for _, path := range []*domain.FileEntryPath{&paths.Source, &paths.TargetParent} {
		for index := range path.Entries {
			path.Entries[index].UserID = maximum
		}
	}
	paths.Source.Entries[len(paths.Source.Entries)-1].ID = maximum
	stats.UserID, stats.EntryID = maximum, maximum
	before, err := json.Marshal([]any{paths, stats})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateEntryMoveLimits(paths, stats); err != nil {
		t.Fatal("完整标识或数量/深度组合边界错误", err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				if err := ValidateEntryMoveLimits(paths, stats); err != nil {
					t.Error("并发限制校验失败", err)
					return
				}
			}
		}()
	}
	workers.Wait()
	after, err := json.Marshal([]any{paths, stats})
	if err != nil || string(before) != string(after) {
		t.Fatal("校验修改了输入", err)
	}
	// 当前父目录关系继续交给后续核心流程结合版本处理。
	paths, stats = entryMoveLimitsFixture(1, "DIR", 3, 1)
	paths.TargetParent.Entries = paths.Source.Entries[:len(paths.Source.Entries)-1]
	if err := ValidateEntryMoveLimits(paths, stats); err != nil {
		t.Fatal("当前父目录的合法限制结果错误", err)
	}
}
