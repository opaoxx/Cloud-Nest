package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func trashEntryPageFixture() (domain.Actor, domain.TrashEntryListItem) {
	actor, reader := fileEntryPathFixture()
	entry := domain.TrashedFileEntry{FileEntry: *reader.entries[13], DeletedAt: reader.entries[13].ModifiedAt}
	entry.Status = "TRASHED"
	return actor, domain.TrashEntryListItem{
		Entry:        entry,
		OriginalPath: domain.FileEntryPath{Entries: []domain.FileEntry{*reader.entries[11], *reader.entries[12]}},
	}
}

func TestTrashEntryPageServiceFlow(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		cursor     string
		include    bool
		nextID     uint64
		wantStages []string
	}{
		{"first and last page", "", true, 0, []string{"list"}},
		{"empty page carries continuation", "", false, 15, []string{"list", "sign"}},
		{"resume from cursor", "previous", true, 12, []string{"verify", "list", "sign"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := t.Context()
			actor, item := trashEntryPageFixture()
			stamp := item.Entry.DeletedAt
			sourceBefore := domain.TrashEntryPosition{ID: 16, DeletedAt: stamp.In(time.FixedZone("UTC+08", 8*60*60))}
			originalBefore := sourceBefore
			listed := &domain.TrashEntryList{}
			if testCase.include {
				listed.Entries = []domain.TrashEntryListItem{item}
			}
			if testCase.nextID != 0 {
				listed.NextPosition = &domain.TrashEntryPosition{ID: testCase.nextID, DeletedAt: stamp}
			}

			// 1. 核对校验、快照列表读取和签发的调用顺序与参数。
			var stages []string
			service := NewTrashEntryPageService(
				func(userID uint64, cursor string) (*domain.TrashEntryPosition, error) {
					stages = append(stages, "verify")
					if userID != actor.UserID || cursor != testCase.cursor {
						t.Fatal("游标校验未收到当前用户和游标原文")
					}
					return &sourceBefore, nil
				},
				func(readContext context.Context, identity domain.Actor, limit int, before *domain.TrashEntryPosition) (*domain.TrashEntryList, error) {
					stages = append(stages, "list")
					if readContext != ctx || identity != actor || limit != 2 {
						t.Fatal("列表服务未收到原上下文、身份及页大小")
					}
					if testCase.cursor == "" {
						if before != nil {
							t.Fatal("首页包含续读位置")
						}
					} else {
						want := domain.TrashEntryPosition{ID: originalBefore.ID, DeletedAt: stamp}
						if before == nil || *before != want {
							t.Fatalf("列表起始位置为 %+v，期望 %+v", before, want)
						}
						before.ID = 0
					}
					return listed, nil
				},
				func(userID uint64, position domain.TrashEntryPosition) (string, error) {
					stages = append(stages, "sign")
					if userID != actor.UserID || position.ID != testCase.nextID || position.DeletedAt != stamp {
						t.Fatalf("签发收到的位置为 %+v", position)
					}
					return "next", nil
				},
			)
			result, err := service.List(ctx, actor, 2, testCase.cursor)
			if err != nil || result == nil || result.Entries == nil || !slices.Equal(stages, testCase.wantStages) ||
				(result.NextCursor != nil) != (testCase.nextID != 0) ||
				(result.NextCursor != nil && *result.NextCursor != "next") {
				t.Fatalf("分页结果为 %+v，阶段为 %v，错误为 %v", result, stages, err)
			}
			if sourceBefore != originalBefore || len(result.Entries) != len(listed.Entries) {
				t.Fatal("调用改变了校验得到的位置或返回了错误数量")
			}

			// 2. 修改返回的路径或父目录指针时，内部列表记录保持自己的值。
			if testCase.include {
				*result.Entries[0].Entry.ParentID = 99
				*result.Entries[0].OriginalPath.Entries[1].ParentID = 98
				result.Entries[0].OriginalPath.Entries[1].Name = "本地改名"
				if *listed.Entries[0].Entry.ParentID != 12 || *listed.Entries[0].OriginalPath.Entries[1].ParentID != 11 ||
					listed.Entries[0].OriginalPath.Entries[1].Name != "课程资料 🌱" {
					t.Fatal("返回结果和列表记录共享可修改路径")
				}
			}
		})
	}
}

func TestTrashEntryPageServiceFailures(t *testing.T) {
	invalidData := domain.ErrInvalidFileEntryData
	invalidCursor := domain.ErrInvalidTrashEntryCursor
	listFailure := errors.New("只读快照失败")
	signFailure := errors.New("签发失败")
	for _, testCase := range []struct {
		name        string
		cursor      string
		limit       int
		want        error
		verifyCalls int
		listCalls   int
		signCalls   int
	}{
		{"invalid page size", "", 101, nil, 0, 0, 0},
		{"overlong cursor", strings.Repeat("x", 1025), 2, invalidCursor, 0, 0, 0},
		{"rejected cursor", "invalid", 2, invalidCursor, 1, 0, 0},
		{"missing verified position", "missing", 2, invalidData, 1, 0, 0},
		{"list fails", "", 2, listFailure, 0, 1, 0},
		{"foreign item", "", 2, invalidData, 0, 1, 0},
		{"foreign parent path", "", 2, invalidData, 0, 1, 0},
		{"unsorted tied entries", "", 2, invalidData, 0, 1, 0},
		{"input position repeated by empty page", "previous", 2, invalidData, 1, 1, 0},
		{"next position skips full page", "", 1, invalidData, 0, 1, 0},
		{"sign fails", "", 2, signFailure, 0, 1, 1},
		{"empty signed cursor", "", 2, nil, 0, 1, 1},
		{"canceled during list", "", 2, context.Canceled, 0, 1, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, item := trashEntryPageFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			before := domain.TrashEntryPosition{ID: 16, DeletedAt: item.Entry.DeletedAt}
			listed := &domain.TrashEntryList{
				Entries:      []domain.TrashEntryListItem{item},
				NextPosition: &domain.TrashEntryPosition{ID: 12, DeletedAt: item.Entry.DeletedAt},
			}
			switch testCase.name {
			case "foreign item":
				listed.Entries[0].Entry.UserID++
			case "foreign parent path":
				listed.Entries[0].OriginalPath.Entries[0].UserID++
			case "unsorted tied entries":
				laterEntry := item
				laterEntry.Entry.ID++
				listed.Entries = append(listed.Entries, laterEntry)
			case "input position repeated by empty page":
				listed.Entries, listed.NextPosition = nil, &before
			}

			// 1. 故障出现后返回空结果，定位流程在哪一个阶段停止。
			verifyCalls, listCalls, signCalls := 0, 0, 0
			service := NewTrashEntryPageService(
				func(uint64, string) (*domain.TrashEntryPosition, error) {
					verifyCalls++
					switch testCase.cursor {
					case "invalid":
						return nil, invalidCursor
					case "missing":
						return nil, nil
					}
					return &before, nil
				},
				func(context.Context, domain.Actor, int, *domain.TrashEntryPosition) (*domain.TrashEntryList, error) {
					listCalls++
					if testCase.name == "list fails" {
						return nil, listFailure
					}
					if testCase.name == "canceled during list" {
						cancel()
					}
					return listed, nil
				},
				func(uint64, domain.TrashEntryPosition) (string, error) {
					signCalls++
					if testCase.name == "sign fails" {
						return "", signFailure
					}
					return "", nil
				},
			)
			result, err := service.List(ctx, actor, testCase.limit, testCase.cursor)
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) ||
				verifyCalls != testCase.verifyCalls || listCalls != testCase.listCalls || signCalls != testCase.signCalls {
				t.Fatalf("分页结果为 %+v，错误为 %v，调用次数为 %d/%d/%d", result, err, verifyCalls, listCalls, signCalls)
			}
		})
	}
}
