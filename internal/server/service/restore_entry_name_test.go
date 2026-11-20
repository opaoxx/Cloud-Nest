package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestResolveRestoreEntryName(t *testing.T) {
	for _, scenario := range []struct {
		name, original, entryType, want string
		keys                            []string
	}{
		{"free original", "笔记.MD", "FILE", "笔记.MD", []string{"笔记.md"}},
		{"first gap", "Cafe\u0301.TXT", "FILE", "Cafe\u0301 (2).TXT", []string{"café.txt", "café (1).txt", "café (2).txt"}},
		{"dotted directory", "资料.v2", "DIR", "资料.v2 (1)", []string{"资料.v2", "资料.v2 (1)"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			originalParentID, actualParentID := uint64(12), uint64(11)
			target := &domain.FileEntry{
				ID: 13, UserID: 7, ParentID: &originalParentID, Name: scenario.original,
				EntryType: scenario.entryType, Status: "TRASHED", Revision: 2,
			}
			ctx := t.Context()
			calls := 0
			chosen, err := ResolveRestoreEntryName(ctx, target, actualParentID, func(queryContext context.Context, userID, parentID uint64, key []byte) (*domain.FileEntry, error) {
				if calls >= len(scenario.keys) || queryContext != ctx || userID != target.UserID || parentID != actualParentID || string(key) != scenario.keys[calls] {
					t.Fatalf("第 %d 次查询的范围或名称键错误，用户 %d，父目录 %d，键 %q", calls+1, userID, parentID, key)
				}
				calls++
				if calls == len(scenario.keys) {
					return nil, fmt.Errorf("fixture: %w", domain.ErrFileEntryNotFound)
				}
				return &domain.FileEntry{ID: uint64(20 + calls), UserID: userID, ParentID: &parentID, Status: "ACTIVE"}, nil
			})
			if chosen != scenario.want || err != nil || calls != len(scenario.keys) || target.Name != scenario.original {
				t.Fatalf("实际名称为 %q，查询 %d 次，错误为 %v，期望 %q", chosen, calls, err, scenario.want)
			}
		})
	}
}

func TestResolveRestoreEntryNameErrors(t *testing.T) {
	parentID := uint64(11)
	otherParentID := uint64(12)
	queryFailure := errors.New("fixture query")
	match := domain.FileEntry{ID: 20, UserID: 7, ParentID: &parentID, Status: "ACTIVE"}
	wrongParent := match
	wrongParent.ParentID = &otherParentID
	wrongUser := match
	wrongUser.UserID++
	trashed := match
	trashed.Status = "TRASHED"

	for _, scenario := range []struct {
		name, storedName string
		lookupEntry      *domain.FileEntry
		lookupError      error
		cancel           bool
		wantCalls        int
		wantError        error
	}{
		{name: "invalid stored name", storedName: "a/b", wantError: domain.ErrInvalidFileEntryData},
		{name: "stored name was not trimmed", storedName: " 笔记.md ", wantError: domain.ErrInvalidFileEntryData},
		{name: "query error", storedName: "笔记.md", lookupError: queryFailure, wantCalls: 1, wantError: queryFailure},
		{name: "contradictory match", storedName: "笔记.md", lookupEntry: &match, lookupError: domain.ErrFileEntryNotFound, wantCalls: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "nil record on success", storedName: "笔记.md", wantCalls: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "other parent", storedName: "笔记.md", lookupEntry: &wrongParent, wantCalls: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "other user", storedName: "笔记.md", lookupEntry: &wrongUser, wantCalls: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "nonactive match", storedName: "笔记.md", lookupEntry: &trashed, wantCalls: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "no space for suffix", storedName: "a." + strings.Repeat("x", 115), lookupEntry: &match, wantCalls: 1, wantError: domain.ErrEntryNameConflict},
		{name: "cancelled during free candidate query", storedName: "笔记.md", lookupEntry: &match, cancel: true, wantCalls: 2, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target := &domain.FileEntry{ID: 13, UserID: 7, Name: scenario.storedName, Status: "TRASHED", EntryType: "FILE", Revision: 2}
			calls := 0
			chosen, err := ResolveRestoreEntryName(ctx, target, parentID, func(context.Context, uint64, uint64, []byte) (*domain.FileEntry, error) {
				calls++
				if calls > scenario.wantCalls {
					t.Fatal("超出预期查询次数")
				}
				if scenario.cancel && calls == 2 {
					cancel()
					return nil, domain.ErrFileEntryNotFound
				}
				return scenario.lookupEntry, scenario.lookupError
			})
			if chosen != "" || !errors.Is(err, scenario.wantError) || calls != scenario.wantCalls {
				t.Fatalf("得到名称 %q、%d 次查询和错误 %v，期望错误 %v", chosen, calls, err, scenario.wantError)
			}
		})
	}
}
