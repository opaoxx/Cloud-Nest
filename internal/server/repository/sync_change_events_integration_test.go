package repository

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestListSyncChangeEventsIntegration(t *testing.T) {
	// 1. 准备独占库，用正式表定义和两个账号交错的事件验证归属与排序。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(writeContext context.Context, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(writeContext, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(ctx, deviceDDL)
	execute(ctx, `INSERT INTO sync_devices(device_id,user_id,ack_sequence,status,cursor_generation,last_seen_at)
		VALUES(19,1,17,'ACTIVE',UNHEX(REPEAT('11',16)),UTC_TIMESTAMP(6))`)
	execute(ctx, "UPDATE users SET change_seq=22 WHERE id=1")
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(?,?,?,?,?,?)"
	const creation = `{"entry_id":"12","parent_id":"11","name":"合成目录","revision":"1"}`
	const rename = `{"entry_id":"12","parent_id":"11","name":"已改名","revision":"2"}`
	execute(ctx, insertEvent, actor.UserID, 22, 12, 2, "ENTRY_RENAMED", rename)
	execute(ctx, insertEvent, actor.UserID, 18, 12, 1, "DIRECTORY_CREATED", creation)
	execute(ctx, insertEvent, actor.UserID, 17, 12, 1, "DIRECTORY_CREATED", creation)
	execute(ctx, insertEvent, actor.UserID, 19, 12, 2, "ENTRY_RENAMED", rename)
	execute(ctx, insertEvent, 2, 18, 22, 1, "DIRECTORY_CREATED", `{"entry_id":"22","parent_id":"21","name":"其他账号","revision":"1"}`)
	execute(ctx, insertEvent, 2, 20, 22, 2, "ENTRY_RENAMED", `{"entry_id":"22","parent_id":"21","name":"其他账号改名","revision":"2"}`)

	assertPage := func(readContext context.Context, reader *Store, position, highWater uint64, limit int, want ...uint64) []domain.SyncChangeEvent {
		t.Helper()
		events, err := reader.ListSyncChangeEvents(readContext, position, highWater, limit)
		sequences := make([]uint64, 0, len(events))
		for _, event := range events {
			sequences = append(sequences, event.Sequence)
		}
		if err != nil || events == nil || !slices.Equal(sequences, want) {
			t.Fatalf("位置 %d，上界 %d，页大小 %d：得到 %v，错误 %v，预期 %v", position, highWater, limit, sequences, err, want)
		}
		return events
	}

	// 2. 验证范围、页大小、原始字段、空页和取消。
	err := store.WithSyncChangePullSnapshot(ctx, actor, func(readContext context.Context, reader *Store, _ domain.SyncDeviceState) error {
		events := assertPage(readContext, reader, 17, 22, 2, 18, 19)
		for index, expected := range []struct {
			revision        uint64
			eventType, name string
		}{
			{1, "DIRECTORY_CREATED", "合成目录"},
			{2, "ENTRY_RENAMED", "已改名"},
		} {
			event := events[index]
			var payload map[string]string
			if err := json.Unmarshal(event.Payload, &payload); err != nil || event.EntryID != 12 ||
				event.Revision != expected.revision || event.EventType != expected.eventType ||
				payload["name"] != expected.name || payload["entry_id"] != "12" {
				t.Fatalf("交付的事件或载荷不符：%+v，载荷 %v，错误 %v", event, payload, err)
			}
		}
		assertPage(readContext, reader, 17, 19, 100, 18, 19)
		assertPage(readContext, reader, 18, 22, 100, 19, 22)
		assertPage(readContext, reader, 19, 22, 1, 22)
		assertPage(readContext, reader, 22, 22, 1)
		for _, limit := range []int{0, 101} {
			if events, err := reader.ListSyncChangeEvents(readContext, 17, 22, limit); events != nil || err == nil {
				t.Fatalf("无效页大小 %d 得到 %v，错误 %v", limit, events, err)
			}
		}
		if events, err := reader.ListSyncChangeEvents(readContext, 23, 22, 1); events != nil || !errors.Is(err, domain.ErrInvalidSyncSnapshotSequence) {
			t.Fatalf("越界起点得到 %v，错误 %v", events, err)
		}
		canceled, stop := context.WithCancel(readContext)
		stop()
		if events, err := reader.ListSyncChangeEvents(canceled, 22, 22, 1); events != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后得到 %v，错误 %v", events, err)
		}

		// 3. 使用第二连接追加事件并提交，当前读视图仍读不到这条新记录。
		execute(readContext, insertEvent, actor.UserID, 23, 12, 2, "ENTRY_RENAMED", rename)
		assertPage(readContext, reader, 22, 23, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	execute(ctx, insertEvent, actor.UserID, uint64(math.MaxUint64), 12, 2, "ENTRY_RENAMED", rename)
	execute(ctx, "UPDATE users SET change_seq=? WHERE id=1", uint64(math.MaxUint64))
	err = store.WithSyncChangePullSnapshot(ctx, actor, func(readContext context.Context, reader *Store, _ domain.SyncDeviceState) error {
		assertPage(readContext, reader, 22, 23, 1, 23)
		assertPage(readContext, reader, math.MaxUint64-1, math.MaxUint64, 1, math.MaxUint64)
		assertPage(readContext, reader, math.MaxUint64, math.MaxUint64, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if events, err := store.ListSyncChangeEvents(ctx, 17, 22, 1); events != nil || err == nil {
		t.Fatalf("普通 Store 得到 %v，错误 %v", events, err)
	}
}
