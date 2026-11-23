package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestReadSyncChangePageInSnapshotIntegration(t *testing.T) {
	// 1. 在独占库中用正式表准备一个已确认设备，以及序号 18～20 的保留日志。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(deviceDDL)
	generation := domain.SyncCursorGeneration{0xf1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 0xfe}
	execute(`INSERT INTO sync_devices(device_id,user_id,ack_sequence,status,cursor_generation,last_seen_at,updated_at)
		VALUES(19,1,17,'ACTIVE',?,'2020-01-01','2020-01-02')`, generation[:])
	execute("UPDATE users SET change_seq=20 WHERE id=1")
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(1,?,12,?,?,?)"
	execute(insertEvent, 18, 1, "DIRECTORY_CREATED", `{"entry_id":"12","parent_id":"11","name":"合成目录","revision":"1"}`)
	execute(insertEvent, 19, 2, "ENTRY_RENAMED", `{"entry_id":"12","parent_id":"11","name":"第一次改名","revision":"2"}`)
	execute(insertEvent, 20, 3, "ENTRY_RENAMED", `{"entry_id":"12","parent_id":"11","name":"第二次改名","revision":"3"}`)

	t.Run("pull service from signed baseline through empty page", func(t *testing.T) {
		// 1. 用合成密钥签发起点，并把服务绑定到真实只读事务。
		key := bytes.Repeat([]byte{0x42}, sha256.Size)
		baselineSigner, err := service.NewSyncSnapshotBaselineSigner(key)
		if err != nil {
			t.Fatal(err)
		}
		baselineVerifier, err := service.NewSyncSnapshotBaselineVerifier(key)
		if err != nil {
			t.Fatal(err)
		}
		cursorSigner, err := service.NewSyncChangeCursorSigner(key)
		if err != nil {
			t.Fatal(err)
		}
		cursorVerifier, err := service.NewSyncChangeCursorVerifier(key)
		if err != nil {
			t.Fatal(err)
		}
		binding := service.SyncSnapshotCacheBinding{
			SnapshotID: uuid.MustParse("315f8253-c80c-4c35-a821-7efece392508"),
			UserID:     actor.UserID, DeviceID: actor.DeviceID, Generation: generation, PageSize: 2,
		}
		token, err := baselineSigner.Sign(binding, 17, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		resolver := service.NewSyncChangePullStartResolver(baselineVerifier, cursorVerifier, time.Now)
		puller := service.NewSyncChangePullService(resolver, func(ctx context.Context, actor domain.Actor,
			read func(context.Context, service.SyncChangePageReader, domain.SyncDeviceState) error,
		) error {
			return store.WithSyncChangePullSnapshot(ctx, actor, func(readContext context.Context, reader *Store, state domain.SyncDeviceState) error {
				return read(readContext, reader, state)
			})
		}, cursorSigner)

		// 2. 依次使用末页凭据和签出的游标，读到末尾再轮询空页。
		first, err := puller.Pull(ctx, actor, token, "", 2)
		if err != nil || first == nil || !first.HasMore || first.HighWaterSequence != 20 || len(first.Events) != 2 ||
			first.Events[0].Sequence != 18 || first.Events[1].Sequence != 19 {
			t.Fatalf("首次拉取结果不符：%+v，错误 %v", first, err)
		}
		next, err := puller.Pull(ctx, actor, "", first.NextCursor, 2)
		if err != nil || next == nil || next.HasMore || next.HighWaterSequence != 20 ||
			len(next.Events) != 1 || next.Events[0].Sequence != 20 {
			t.Fatalf("末页拉取结果不符：%+v，错误 %v", next, err)
		}
		empty, err := puller.Pull(ctx, actor, "", next.NextCursor, 2)
		if err != nil || empty == nil || empty.HasMore || empty.HighWaterSequence != 20 ||
			empty.Events == nil || len(empty.Events) != 0 || empty.NextCursor != next.NextCursor {
			t.Fatalf("空页轮询结果不符：%+v，错误 %v", empty, err)
		}
	})

	start := service.SyncChangePullStart{Position: domain.SyncChangeCursorPosition{
		UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: 17,
	}}
	read := func(callContext context.Context, start service.SyncChangePullStart, pageSize int, clock func() time.Time) (*service.SyncChangeReadPage, error) {
		var page *service.SyncChangeReadPage
		err := store.WithSyncChangePullSnapshot(callContext, actor, func(readContext context.Context, reader *Store, state domain.SyncDeviceState) error {
			var readError error
			page, readError = service.ReadSyncChangePageInSnapshot(readContext, actor, &start, state, pageSize, clock, reader)
			return readError
		})
		if err != nil {
			return nil, err
		}
		return page, nil
	}
	assertPage := func(page *service.SyncChangeReadPage, err error, highWater uint64, want ...uint64) {
		t.Helper()
		if err != nil || page == nil || page.HighWaterSequence != highWater || page.Events == nil {
			t.Fatalf("增量页不符：%+v，错误 %v，预期上界 %d", page, err, highWater)
		}
		sequences := make([]uint64, 0, len(page.Events))
		for _, event := range page.Events {
			sequences = append(sequences, event.Sequence)
		}
		if !slices.Equal(sequences, want) {
			t.Fatalf("事件序号为 %v，预期 %v", sequences, want)
		}
	}

	// 2. 在读完旧边界后，用第二个连接提交头部裁剪和末尾追加。
	baseline := start
	baseline.BaselineExpiresAt = time.Now().Add(time.Minute)
	page, err := read(ctx, baseline, 2, time.Now)
	assertPage(page, err, 20, 18, 19)
	page, err = read(ctx, start, 2, func() time.Time {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "DELETE FROM change_events WHERE user_id=1 AND `sequence`=18"); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ExecContext(ctx, insertEvent, 21, 4, "ENTRY_RENAMED", `{"entry_id":"12","parent_id":"11","name":"并发提交","revision":"4"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ExecContext(ctx, "UPDATE users SET change_seq=21 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		return time.Now()
	})
	assertPage(page, err, 20, 18, 19)
	if page, err := read(ctx, start, 2, time.Now); page != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
		t.Fatalf("新视图应拒绝已裁掉的起点：%+v，错误 %v", page, err)
	}
	start.Position.Sequence = 18
	page, err = read(ctx, start, 4, time.Now)
	assertPage(page, err, 21, 19, 20, 21)

	// 3. 验证正常空页、恰到凭据期限、取消和日志损坏都遵循交付边界。
	emptyStart := start
	emptyStart.Position.Sequence = 21
	page, err = read(ctx, emptyStart, 1, time.Now)
	assertPage(page, err, 21)
	expired := baseline
	expired.Position.Sequence = 18
	expired.BaselineExpiresAt = time.Now()
	if page, err := read(ctx, expired, 1, func() time.Time { return expired.BaselineExpiresAt }); page != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
		t.Fatalf("到期时仍交付增量页：%+v，错误 %v", page, err)
	}
	canceled, stop := context.WithCancel(ctx)
	defer stop()
	if page, err := read(canceled, emptyStart, 1, func() time.Time {
		stop()
		return time.Now()
	}); page != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后仍交付增量页：%+v，错误 %v", page, err)
	}
	execute("UPDATE change_events SET payload='{}' WHERE user_id=1 AND `sequence`=20")
	if page, err := read(ctx, start, 4, time.Now); page != nil || !errors.Is(err, domain.ErrInvalidSyncChangePage) ||
		!strings.Contains(err.Error(), "序号 20") {
		t.Fatalf("损坏的事件页应整页失败：%+v，错误 %v", page, err)
	}
	var unchanged int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_devices WHERE device_id=19 AND ack_sequence=17
		AND status='ACTIVE' AND cursor_generation=? AND last_seen_at='2020-01-01' AND updated_at='2020-01-02'`, generation[:]).
		Scan(&unchanged); err != nil || unchanged != 1 {
		t.Fatalf("设备状态、位置或时间字段发生变化：匹配 %d 行，错误 %v", unchanged, err)
	}
}
