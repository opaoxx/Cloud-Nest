package domain

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestGlobalEntryQueryValidCombinations(t *testing.T) {
	// 1. 穷举方案中的全局范围、实际排序和全部分类。
	for _, mode := range []struct{ scope, sort string }{
		{GlobalEntryScopeAll, GlobalEntrySortModifiedDesc},
		{GlobalEntryScopeAll, GlobalEntrySortNameAsc},
		{GlobalEntryScopeAll, GlobalEntrySortSizeDesc},
		{GlobalEntryScopeStarred, GlobalEntrySortModifiedDesc},
		{GlobalEntryScopeStarred, GlobalEntrySortNameAsc},
		{GlobalEntryScopeStarred, GlobalEntrySortSizeDesc},
		{GlobalEntryScopeRecent, GlobalEntrySortLastOpenedDesc},
	} {
		for _, kind := range []string{"", "folder", "document", "image", "video", "audio", "archive", "other"} {
			t.Run(mode.scope+"/"+mode.sort+"/kind="+kind, func(t *testing.T) {
				query := GlobalEntryQuery{Scope: mode.scope, Sort: mode.sort, Kind: kind, PageSize: 50}
				if !query.IsValid() {
					t.Fatal("方案中的查询组合被拒绝", query)
				}
			})
		}
	}
}

func TestGlobalEntryQueryBoundsAndSearch(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		size   int
		search string
		valid  bool
	}{
		{name: "minimum page", size: 1, valid: true},
		{name: "defaulted page", size: 50, valid: true},
		{name: "maximum page", size: 100, valid: true},
		{name: "zero page", size: 0},
		{name: "negative page", size: -1},
		{name: "large page", size: 101},
		{name: "largest int page", size: int(^uint(0) >> 1)},
		{name: "Chinese and emoji", size: 50, search: "学习笔记 🌱", valid: true},
		{name: "literal whitespace", size: 50, search: "  学习笔记 \t\n", valid: true},
		{name: "literal pattern characters", size: 50, search: `%_\\'"`, valid: true},
		{name: "decomposed Unicode", size: 50, search: "Cafe\u0301", valid: true},
		{name: "long UTF-8 text", size: 50, search: strings.Repeat("学习🌱", 1024), valid: true},
		{name: "embedded zero", size: 50, search: "笔记\x00", valid: true},
		{name: "invalid UTF-8", size: 50, search: string([]byte{0xff})},
		{name: "truncated UTF-8", size: 50, search: string([]byte{0xe4, 0xb8})},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 验证边界与编码，核对搜索原文及查询结构。
			query := GlobalEntryQuery{Scope: GlobalEntryScopeAll, Sort: GlobalEntrySortModifiedDesc, Search: testCase.search, PageSize: testCase.size}
			before := query
			if query.IsValid() != testCase.valid || !reflect.DeepEqual(query, before) {
				t.Fatal("页大小、搜索校验或输入保护错误")
			}
		})
	}
}

func TestGlobalEntryQueryInvalidCombinations(t *testing.T) {
	for _, testCase := range []struct{ name, scope, sort, kind string }{
		{"empty scope", "", GlobalEntrySortModifiedDesc, ""},
		{"directory scope", "directory", GlobalEntrySortModifiedDesc, ""},
		{"unknown scope", "private-scope", GlobalEntrySortModifiedDesc, ""},
		{"uppercase scope", "ALL", GlobalEntrySortModifiedDesc, ""},
		{"padded scope", "all ", GlobalEntrySortModifiedDesc, ""},
		{"empty sort", GlobalEntryScopeAll, "", ""},
		{"unknown sort", GlobalEntryScopeAll, "private-sort", ""},
		{"uppercase sort", GlobalEntryScopeAll, "MODIFIED_DESC", ""},
		{"padded sort", GlobalEntryScopeAll, " modified_desc", ""},
		{"all access sort", GlobalEntryScopeAll, GlobalEntrySortLastOpenedDesc, ""},
		{"starred access sort", GlobalEntryScopeStarred, GlobalEntrySortLastOpenedDesc, ""},
		{"recent modified sort", GlobalEntryScopeRecent, GlobalEntrySortModifiedDesc, ""},
		{"recent name sort", GlobalEntryScopeRecent, GlobalEntrySortNameAsc, ""},
		{"recent size sort", GlobalEntryScopeRecent, GlobalEntrySortSizeDesc, ""},
		{"recent empty sort", GlobalEntryScopeRecent, "", ""},
		{"unknown kind", GlobalEntryScopeAll, GlobalEntrySortModifiedDesc, "private-kind"},
		{"all kind", GlobalEntryScopeAll, GlobalEntrySortModifiedDesc, "all"},
		{"raw directory type", GlobalEntryScopeStarred, GlobalEntrySortModifiedDesc, "DIR"},
		{"raw file type", GlobalEntryScopeAll, GlobalEntrySortModifiedDesc, "FILE"},
		{"raw category", GlobalEntryScopeRecent, GlobalEntrySortLastOpenedDesc, "DOCUMENT"},
		{"padded kind", GlobalEntryScopeAll, GlobalEntrySortModifiedDesc, "document "},
		{"unknown recent kind", GlobalEntryScopeRecent, GlobalEntrySortLastOpenedDesc, "private-kind"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 非法组合整体失败，输入按原值保留。
			query := GlobalEntryQuery{Scope: testCase.scope, Sort: testCase.sort, Kind: testCase.kind, PageSize: 50}
			before := query
			if query.IsValid() || query != before {
				t.Fatal("非法条件被接受或修改了输入")
			}
		})
	}
	if (GlobalEntryQuery{}).IsValid() {
		t.Fatal("未准备默认值的零值查询通过校验")
	}
}

func TestGlobalEntryQueryValueAndConcurrentChecks(t *testing.T) {
	// 1. 查询按值传递，副本调整页大小与筛选后原查询可独立复用。
	original := GlobalEntryQuery{Scope: GlobalEntryScopeStarred, Sort: GlobalEntrySortNameAsc, Search: " 笔记%_ 🌱 ", Kind: "document", PageSize: 50}
	before := original
	queryCopy := original
	queryCopy.Scope, queryCopy.Sort, queryCopy.Search, queryCopy.Kind, queryCopy.PageSize = GlobalEntryScopeRecent, GlobalEntrySortLastOpenedDesc, "最近", "", 100
	if !original.IsValid() || !queryCopy.IsValid() || original != before {
		t.Fatal("查询副本与原值互相影响")
	}
	// 2. 多个读取方共享同一只读条件，重复检查结果准确。
	invalid := original
	invalid.PageSize = 0
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				if !original.IsValid() || invalid.IsValid() || original != before {
					t.Error("并发校验结果或查询原值错误")
				}
			}
		})
	}
	workers.Wait()
	if MaxGlobalEntryCandidates != 1000 {
		t.Fatal("候选扫描预算与已批准方案不符")
	}
}
