package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReadOfflineSyncDeviceCutoffIntegration(t *testing.T) {
	verifyDatabaseNinetyDayCutoff(t, (*Store).ReadOfflineSyncDeviceCutoff)
}

func TestReadSyncChangePruneCutoffIntegration(t *testing.T) {
	verifyDatabaseNinetyDayCutoff(t, (*Store).ReadSyncChangePruneCutoff)
}

func verifyDatabaseNinetyDayCutoff(t *testing.T, readCutoff func(*Store, context.Context) (time.Time, error)) {
	t.Helper()
	// 1. 在独占数据库连接中固定非 UTC 会话和带微秒的数据库时钟。
	pool := loginTransactionDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	execute("SET time_zone = '+05:30'")
	defer execute("SET time_zone = '+00:00'")
	defer execute("SET timestamp = DEFAULT")
	// 1/64 秒可用二进制精确表示，且尾数 625 微秒能检出毫秒截断。
	reference := time.Date(2026, 11, 23, 12, 0, 0, 15625000, time.UTC)
	execute("SET timestamp = ?", float64(reference.Unix())+0.015625)
	var sessionTime, utcTime time.Time
	err = pool.QueryRowContext(ctx, "SELECT CURRENT_TIMESTAMP(6), UTC_TIMESTAMP(6)").Scan(&sessionTime, &utcTime)
	if err != nil || !utcTime.Equal(reference) || !sessionTime.Equal(reference.Add(5*time.Hour+30*time.Minute)) {
		t.Fatalf("隔离时钟夹具不符：会话 %v，UTC %v，错误 %v", sessionTime, utcTime, err)
	}

	// 2. 核对从 UTC 减 90 天的结果。再次读取可得到下一轮的截止点。
	cutoff, err := readCutoff(store, ctx)
	want := reference.AddDate(0, 0, -90)
	if err != nil || cutoff != want {
		t.Fatalf("UTC 截止点不符：实际 %v，期望 %v，错误 %v", cutoff, want, err)
	}
	execute("SET timestamp = ?", float64(reference.Unix())+1.015625)
	next, err := readCutoff(store, ctx)
	if err != nil || next != want.Add(time.Second) || cutoff != want {
		t.Fatalf("重新读取的截止点不符：本轮 %v，下一轮 %v，错误 %v", cutoff, next, err)
	}

	// 3. 取消时交付原错误，不返回可被下一轮误用的时间。
	canceled, stop := context.WithCancel(ctx)
	stop()
	if result, err := readCutoff(store, canceled); !result.IsZero() || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消时应返回零值和取消原因：%v，错误 %v", result, err)
	}
	t.Logf("非 UTC 会话 +05:30，数据库 UTC %s，截止点 %s，时钟推进一秒后为 %s",
		reference.Format(time.RFC3339Nano), cutoff.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
}
