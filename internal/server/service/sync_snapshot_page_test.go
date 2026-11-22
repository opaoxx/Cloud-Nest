package service

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func syncSnapshotForPageTest(t *testing.T, count, pageSize int) (*CachedSyncSnapshot, []json.RawMessage) {
	t.Helper()
	stamp := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	cache := NewSyncSnapshotCache(func() time.Time { return stamp })
	binding, data := syncSnapshotCacheFixture()
	binding.PageSize = pageSize
	data.Entries.Items = make([]json.RawMessage, count)
	for index := range data.Entries.Items {
		entryID := strconv.Itoa(12 + index)
		data.Entries.Items[index] = json.RawMessage(strings.Replace(snapshotCacheFixtureItem, `"entry_id":"12"`, `"entry_id":"`+entryID+`"`, 1))
	}
	encoded, err := json.Marshal(data.Entries.Items)
	if err != nil {
		t.Fatal(err)
	}
	data.Entries.SizeBytes = len(encoded)
	putSyncSnapshotForReadTest(t, cache, binding, data)
	snapshot, err := cache.Get(t.Context(), binding)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, data.Entries.Items
}

func TestCachedSyncSnapshotPageReadsFixedRanges(t *testing.T) {
	// 1. 使用缓存保存的页大小依次读取，核对原顺序、末页和准确的下一位置。
	for _, testCase := range []struct {
		name     string
		count    int
		pageSize int
		lengths  []int
	}{
		{"first middle and short last", 5, 2, []int{2, 2, 1}},
		{"full last", 4, 2, []int{2, 2}},
		{"minimum page size", 2, 1, []int{1, 1}},
		{"maximum page size", 101, 100, []int{100, 1}},
		{"empty tree", 0, 2, []int{0}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, items := syncSnapshotForPageTest(t, testCase.count, testCase.pageSize)
			position := 0
			for pageIndex, expectedCount := range testCase.lengths {
				page, err := snapshot.Page(position)
				if err != nil || page == nil || page.Items == nil || len(page.Items) != expectedCount {
					t.Fatalf("位置 %d 的条目数预期为 %d，错误=%v", position, expectedCount, err)
				}
				for index, item := range page.Items {
					if string(item) != string(items[position+index]) {
						t.Fatalf("位置 %d 的条目或顺序改变", position+index)
					}
				}
				position += expectedCount
				if pageIndex == len(testCase.lengths)-1 {
					if page.NextPosition != nil {
						t.Fatal("末页不应给出下一位置")
					}
				} else if page.NextPosition == nil || *page.NextPosition != position {
					t.Fatalf("下一页起点预期为 %d", position)
				}
			}
			if position != len(items) {
				t.Fatal("未完整覆盖快照条目")
			}
		})
	}
}

func TestCachedSyncSnapshotPageRejectsInvalidPositions(t *testing.T) {
	// 1. 拒绝负数、未对齐的位置、末尾额外空页和过大的起点。
	snapshot, _ := syncSnapshotForPageTest(t, 4, 2)
	for _, position := range []int{-1, 1, 3, 4, 6, math.MaxInt, math.MaxInt - 1} {
		if page, err := snapshot.Page(position); page != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
			t.Fatalf("非法位置 %d 应拒绝，错误=%v", position, err)
		}
	}
	empty, _ := syncSnapshotForPageTest(t, 0, 2)
	if page, err := empty.Page(2); page != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotCursor) {
		t.Fatalf("空树仅允许读取起点零，错误=%v", err)
	}
}

func TestCachedSyncSnapshotPageReplayReturnsIndependentItems(t *testing.T) {
	// 1. 修改某次返回值，确认同一位置再次读取仍保留原条目及原下一位置。
	snapshot, items := syncSnapshotForPageTest(t, 3, 2)
	first, err := snapshot.Page(0)
	if err != nil || first == nil || first.NextPosition == nil || len(first.Items) != 2 {
		t.Fatalf("首页结果不符，错误=%v", err)
	}
	first.Items[0][0] = '['
	first.Items[1] = nil
	*first.NextPosition = 1
	second, err := snapshot.Page(0)
	if err != nil || second == nil || second.NextPosition == nil || *second.NextPosition != 2 || len(second.Items) != 2 ||
		string(second.Items[0]) != string(items[0]) || string(second.Items[1]) != string(items[1]) {
		t.Fatalf("重读同页时未取得原数据和原位置，错误=%v", err)
	}
}
