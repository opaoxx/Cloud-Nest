package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListUserTrashEntryCandidatesInput(t *testing.T) {
	ctx := t.Context()
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	for _, testCase := range []struct {
		name   string
		ctx    context.Context
		userID uint64
		limit  int
		before *domain.TrashEntryPosition
	}{
		{name: "nil context", userID: 7, limit: 1},
		{name: "zero user", ctx: ctx, limit: 1},
		{name: "zero limit", ctx: ctx, userID: 7},
		{name: "negative limit", ctx: ctx, userID: 7, limit: -1},
		{name: "above maximum limit", ctx: ctx, userID: 7, limit: 101},
		{name: "zero position ID", ctx: ctx, userID: 7, limit: 1, before: &domain.TrashEntryPosition{DeletedAt: stamp}},
		{name: "zero position time", ctx: ctx, userID: 7, limit: 1, before: &domain.TrashEntryPosition{ID: 12}},
		{name: "position with extra precision", ctx: ctx, userID: 7, limit: 1, before: &domain.TrashEntryPosition{ID: 12, DeletedAt: stamp.Add(time.Nanosecond)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 无效参数应在访问数据库之前返回。
			store := &Store{}
			if page, err := store.ListUserTrashEntryCandidates(testCase.ctx, testCase.userID, testCase.limit, testCase.before); page != nil || err == nil {
				t.Fatalf("无效参数返回了候选页或没有返回错误，页为 %+v，错误为 %v", page, err)
			}
		})
	}

	// 2. 取消也应在读取前返回，并保留原始原因。
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	store := &Store{}
	if page, err := store.ListUserTrashEntryCandidates(canceled, 7, 1, nil); page != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消结果不符，页为 %+v，错误为 %v", page, err)
	}
}

type trashEntryCandidateRows struct{ *directoryEntryRows }

func (*trashEntryCandidateRows) Columns() []string {
	return append((&fileEntryRows{}).Columns(), "deleted_at")
}

func TestListUserTrashEntryCandidatesRejectsInvalidRows(t *testing.T) {
	// 1. 用测试驱动模拟生产约束通常会阻止的异常记录和读取失败。
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	entry := domain.FileEntry{
		ID: 15, UserID: 7, ParentID: &parentID, Name: "回收的目录", EntryType: "DIR",
		Revision: 2, Status: "TRASHED", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp,
	}
	valid := append(directoryEntryValues(entry), stamp)
	foreign := append(directoryEntryValues(entry), stamp)
	foreign[1] = uint64(8)
	missingTime := append(directoryEntryValues(entry), nil)
	rowError := errors.New("测试驱动读取中断")
	for _, testCase := range []struct {
		name     string
		records  [][]driver.Value
		before   *domain.TrashEntryPosition
		rowError error
		want     error
	}{
		{name: "foreign row", records: [][]driver.Value{foreign}, want: domain.ErrInvalidFileEntryData},
		{name: "missing deletion time in extra row", records: [][]driver.Value{valid, missingTime}, want: domain.ErrInvalidFileEntryData},
		{name: "duplicate position", records: [][]driver.Value{valid, valid}, want: domain.ErrInvalidFileEntryData},
		{name: "exclusive upper bound", records: [][]driver.Value{valid}, before: &domain.TrashEntryPosition{ID: 15, DeletedAt: stamp}, want: domain.ErrInvalidFileEntryData},
		{name: "partial read error", records: [][]driver.Value{valid}, rowError: rowError, want: rowError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			connector := &deviceConnector{accountConnector: accountConnector{
				query: func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
					return &trashEntryCandidateRows{&directoryEntryRows{records: testCase.records, terminalError: testCase.rowError}}, nil
				},
			}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 拒绝整批结果，保留错误原因。
			page, err := store.ListUserTrashEntryCandidates(t.Context(), 7, 1, testCase.before)
			if page != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("异常候选批次为 %+v，错误为 %v，期望 %v", page, err, testCase.want)
			}
		})
	}
}
