package repository

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestNewRestoredSyncStoreIntegration(t *testing.T) {
	// 1. 在独占库准备一台合成设备，保留第二条普通连接用于独立读回。
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	pool, poolStore, _ := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	for _, statement := range []string{
		deviceDDL,
		"UPDATE users SET change_seq=7 WHERE id=1",
		"INSERT INTO sync_devices(device_id,user_id,status,cursor_generation,ack_sequence,last_seen_at) VALUES(19,1,'ACTIVE',UNHEX(REPEAT('11',16)),7,UTC_TIMESTAMP(6))",
		`CREATE TRIGGER fixture_pinned_sync_reset BEFORE UPDATE ON sync_devices FOR EACH ROW
			BEGIN IF NOT (@cloudnest_fixture_connection <=> CONNECTION_ID()) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture requires the checked connection'; END IF; END`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var instanceText, databaseName string
	if err := pool.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid, DATABASE()").Scan(&instanceText, &databaseName); err != nil {
		t.Fatal(err)
	}
	instanceUUID, err := uuid.Parse(instanceText)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "SET @cloudnest_fixture_connection = CONNECTION_ID()"); err != nil {
		t.Fatal(err)
	}
	var checkedConnectionID, otherConnectionID uint64
	if err := connection.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&checkedConnectionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&otherConnectionID); err != nil || otherConnectionID == checkedConnectionID {
		t.Fatalf("未取得区别于专用连接的普通连接 %d，%v", otherConnectionID, err)
	}

	// 2. 核对失败无法创建恢复仓储，成功时扫描和换代沿用专用连接。
	if store, err := NewRestoredSyncStore(ctx, connection, instanceUUID, databaseName+"_other"); store != nil ||
		!errors.Is(err, ErrRestoredSyncTargetMismatch) {
		t.Fatalf("目标不匹配时装配结果不符 %v，%v", store != nil, err)
	}
	store, err := NewRestoredSyncStore(ctx, connection, instanceUUID, databaseName)
	if err != nil {
		t.Fatal(err)
	}
	var repositoryConnectionID uint64
	if err := store.database.WithContext(ctx).Raw("SELECT CONNECTION_ID()").Row().Scan(&repositoryConnectionID); err != nil ||
		store.database.Statement.ConnPool != connection || repositoryConnectionID != checkedConnectionID {
		t.Fatalf("恢复仓储没有沿用已核对连接 %d，%v", repositoryConnectionID, err)
	}
	candidates, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 2)
	if err != nil || len(candidates) != 1 || candidates[0].DeviceID != 19 {
		t.Fatalf("专用连接未读到预期候选 %+v，%v", candidates, err)
	}
	var fixtureError *mysql.MySQLError
	if err := poolStore.ResetRestoredSyncDevice(ctx, candidates[0]); !errors.As(err, &fixtureError) || fixtureError.Number != 1644 {
		t.Fatalf("测试触发器应拒绝其他连接的换代 %v", err)
	}
	if err := store.ResetRestoredSyncDevice(ctx, candidates[0]); err != nil {
		t.Fatalf("换代事务未在已核对的连接提交 %v", err)
	}
	readBack, err := poolStore.ListRestoredSyncDeviceCandidates(ctx, 0, 2)
	if err != nil || len(readBack) != 1 || readBack[0].State.Status != domain.SyncDeviceStatusResetRequired ||
		readBack[0].State.AckSequence != 0 || readBack[0].State.Generation == candidates[0].State.Generation {
		t.Fatalf("独立连接未读到已提交的换代 %+v，%v", readBack, err)
	}

	// 3. 关闭专用连接后，旧仓储报错，普通连接仍可读取已提交的记录。
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 2); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("专用连接关闭后扫描应失败 %v", err)
	}
	if err := store.ResetRestoredSyncDevice(ctx, readBack[0]); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("专用连接关闭后换代应失败 %v", err)
	}
	if afterClose, err := poolStore.ListRestoredSyncDeviceCandidates(ctx, 0, 2); err != nil || !slices.Equal(afterClose, readBack) {
		t.Fatalf("连接关闭后发生了意外写入 %+v，%v", afterClose, err)
	}
	t.Log("恢复仓储沿用已核对连接，扫描与事务提交成功，专用连接关闭后读写均报错")
}
