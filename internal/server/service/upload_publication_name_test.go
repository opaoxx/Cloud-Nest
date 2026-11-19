package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestResolveUploadPublicationName(t *testing.T) {
	longBase := strings.Repeat("a", 116)
	for _, scenario := range []struct {
		name, request, want string
		keys                []string
	}{
		{"original is free", "  笔记.TXT ", "笔记.TXT", []string{"笔记.txt"}},
		{"first numbered candidate", "archive.tar.GZ", "archive.tar (1).GZ", []string{"archive.tar.gz", "archive.tar (1).gz"}},
		{"first gap in the sequence", "Straße.TXT", "Straße (2).TXT", []string{"strasse.txt", "strasse (1).txt", "strasse (2).txt"}},
		{"normalization preserves display spelling", "Cafe\u0301.txt", "Cafe\u0301 (1).txt", []string{"café.txt", "café (1).txt"}},
		{"truncate from the original base", longBase + ".txt", strings.Repeat("a", 112) + " (2).txt",
			[]string{longBase + ".txt", strings.Repeat("a", 112) + " (1).txt", strings.Repeat("a", 112) + " (2).txt"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			const userID, parentID = uint64(7), uint64(13)
			calls := 0
			actual, err := ResolveUploadPublicationName(ctx, userID, parentID, scenario.request,
				func(queryContext context.Context, user, parent uint64, key []byte) (*domain.FileEntry, error) {
					if calls >= len(scenario.keys) || queryContext != ctx || user != userID || parent != parentID || string(key) != scenario.keys[calls] {
						t.Fatalf("查询 %d 的上下文、范围或名称键错误，得到 %q", calls+1, key)
					}
					calls++
					if calls == len(scenario.keys) {
						return nil, fmt.Errorf("fixture missing: %w", domain.ErrFileEntryNotFound)
					}
					return &domain.FileEntry{ID: 17, UserID: user, ParentID: &parent, Status: "ACTIVE"}, nil
				})
			if err != nil || actual != scenario.want || calls != len(scenario.keys) {
				t.Fatalf("得到名称 %q、%d 次查询和错误 %v，期望 %q", actual, calls, err, scenario.want)
			}
		})
	}
}

func TestResolveUploadPublicationNameErrors(t *testing.T) {
	queryError := errors.New("fixture query failure")
	parentID := uint64(13)
	occupied := domain.FileEntry{ID: 17, UserID: 7, ParentID: &parentID, Status: "ACTIVE"}
	wrongScope := occupied
	wrongScope.UserID = 8
	for _, scenario := range []struct {
		name, request string
		entry         *domain.FileEntry
		queryError    error
		cancel        bool
		wantError     error
		wantCalls     int
	}{
		{name: "invalid name", request: "bad/path.txt", wantError: domain.ErrInvalidEntryName},
		{name: "query failure", request: "笔记.txt", queryError: queryError, wantError: queryError, wantCalls: 1},
		{name: "missing record with nil error", request: "笔记.txt", wantError: domain.ErrInvalidFileEntryData, wantCalls: 1},
		{name: "contradictory not found", request: "笔记.txt", entry: &occupied, queryError: domain.ErrFileEntryNotFound, wantError: domain.ErrInvalidFileEntryData, wantCalls: 1},
		{name: "foreign match", request: "笔记.txt", entry: &wrongScope, wantError: domain.ErrInvalidFileEntryData, wantCalls: 1},
		{name: "no space for suffix", request: "a." + strings.Repeat("x", 115), entry: &occupied, wantError: domain.ErrEntryNameConflict, wantCalls: 1},
		{name: "cancellation while checking candidate", request: "笔记.txt", entry: &occupied, cancel: true, wantError: context.Canceled, wantCalls: 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			actual, err := ResolveUploadPublicationName(ctx, 7, parentID, scenario.request,
				func(context.Context, uint64, uint64, []byte) (*domain.FileEntry, error) {
					calls++
					if calls > scenario.wantCalls {
						t.Fatal("发生了额外名称查询")
					}
					if scenario.cancel && calls == 2 {
						cancel()
						return nil, domain.ErrFileEntryNotFound
					}
					return scenario.entry, scenario.queryError
				})
			if actual != "" || !errors.Is(err, scenario.wantError) || calls != scenario.wantCalls {
				t.Fatalf("得到名称 %q、%d 次查询和错误 %v，期望错误 %v", actual, calls, err, scenario.wantError)
			}
		})
	}
}
