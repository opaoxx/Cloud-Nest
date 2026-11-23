package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
)

func TestCheckRestoredSyncTargetIntegration(t *testing.T) {
	// 1. 从专用实例的目标库记录身份，并建立同实例的另一独占库。
	targetPool := loginTransactionDatabase(t)
	otherPool := loginTransactionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var instanceUUIDText, targetName, otherName string
	if err := targetPool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid, DATABASE()").Scan(&instanceUUIDText, &targetName); err != nil {
		t.Fatal(err)
	}
	if err := otherPool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&otherName); err != nil || otherName == targetName {
		t.Fatalf("另一独占库身份不符 %q，%v", otherName, err)
	}
	instanceUUID, err := uuid.Parse(instanceUUIDText)
	if err != nil || instanceUUID == uuid.Nil {
		t.Fatalf("专用 MySQL 的实例 UUID 无效 %q，%v", instanceUUIDText, err)
	}
	targetConnection, err := targetPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer targetConnection.Close()
	otherConnection, err := otherPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer otherConnection.Close()

	// 2. 比对正确连接、实际连到其他库，以及实际实例 UUID 不合预期的情况。
	if err := CheckRestoredSyncTarget(ctx, targetConnection, instanceUUID, targetName); err != nil {
		t.Fatalf("正确目标应通过核对 %v", err)
	}
	if err := CheckRestoredSyncTarget(ctx, otherConnection, instanceUUID, targetName); !errors.Is(err, ErrRestoredSyncTargetMismatch) {
		t.Fatalf("同实例其他库应被拒绝 %v", err)
	}
	otherUUID := instanceUUID
	otherUUID[15] ^= 1
	if err := CheckRestoredSyncTarget(ctx, targetConnection, otherUUID, targetName); !errors.Is(err, ErrRestoredSyncTargetMismatch) {
		t.Fatalf("库名正确时仍应拒绝与预期不符的实例 UUID %v", err)
	}

	// 3. 确认未选择库、取消或连接关闭时都不能通过核对。
	withoutSchema, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	withoutSchema.DatabaseName = ""
	withoutSchemaPool, err := OpenMySQL(ctx, withoutSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer withoutSchemaPool.Close()
	withoutSchemaConnection, err := withoutSchemaPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer withoutSchemaConnection.Close()
	if err := CheckRestoredSyncTarget(ctx, withoutSchemaConnection, instanceUUID, targetName); !errors.Is(err, ErrRestoredSyncTargetMismatch) {
		t.Fatalf("没有当前库时应拒绝 %v", err)
	}
	canceled, cancelQuery := context.WithCancel(ctx)
	cancelQuery()
	if err := CheckRestoredSyncTarget(canceled, targetConnection, instanceUUID, targetName); !errors.Is(err, context.Canceled) {
		t.Fatalf("应保留取消信号 %v", err)
	}
	if err := otherConnection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckRestoredSyncTarget(ctx, otherConnection, instanceUUID, targetName); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("应保留数据库连接错误 %v", err)
	}
	var rows, probeTotal int
	if err := targetConnection.QueryRowContext(ctx, "SELECT COUNT(*), SUM(probe_value) FROM users").Scan(&rows, &probeTotal); err != nil ||
		rows != 3 || probeTotal != 0 {
		t.Fatalf("只读核对后合成用户数据不符 %d，%d，%v", rows, probeTotal, err)
	}
	t.Log("实例和库名一致时通过，同实例其他库、实例 UUID 不符、未选库、取消和已关闭连接均被拒绝")
}
