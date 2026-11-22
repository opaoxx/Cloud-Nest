package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func syncDeviceGenerationDatabase(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	// 1. 在独占库中装入正式同步设备表和一个合成账号及其设备。
	pool := userProfileDatabase(t)
	_, syncDeviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到第 3 版同步设备表定义")
	}
	for _, statement := range []string{
		syncDeviceDDL,
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(7,'cursor_fixture','Cursor fixture','fake_hash')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,7,'d87105e2-6d67-493f-8810-36018043864d','Fixture','WEB')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备同步设备夹具失败", err)
		}
	}
	return pool
}

func TestSyncSnapshotCursorGenerationIntegration(t *testing.T) {
	// 1. 写入含零字节及高位字节的代次，从正式查询入口读回。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool := syncDeviceGenerationDatabase(t, ctx)
	stored := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	if _, err := pool.ExecContext(ctx, "INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at) VALUES(19,7,?,UTC_TIMESTAMP(6))", stored[:]); err != nil {
		t.Fatal("写入代次夹具失败", err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.FindSyncDeviceGeneration(ctx, 7, 19)
	if err != nil || generation != stored {
		t.Fatal("未能从正式 BINARY(16) 字段读回完整代次", err)
	}

	// 2. 以数据库读回的完整值签发并校验，比较还原的每一个字节。
	master := bytes.Repeat([]byte{0x67}, sha256.Size)
	signer, err := service.NewSyncSnapshotCursorSigner(master)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncSnapshotCursorVerifier(master)
	if err != nil {
		t.Fatal(err)
	}
	binding := service.SyncSnapshotCacheBinding{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"), UserID: 7, DeviceID: 19, PageSize: 50,
		Generation: generation,
	}
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 123456789, time.UTC)
	credential, err := signer.Sign(binding, 50, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, credential, expiresAt.Add(-time.Nanosecond))
	if err != nil || verified == nil || verified.Binding != binding || verified.Binding.Generation != stored {
		t.Fatal("从凭据还原的代次与正式数据库字段不一致", err)
	}
}

func TestFindSyncDeviceGenerationIntegration(t *testing.T) {
	// 1. 准备已初始化、未初始化、已撤销和数据异常的不同设备。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool := syncDeviceGenerationDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("建立同步查询夹具失败", err)
		}
	}
	exec("INSERT INTO users(id,username,display_name,password_hash) VALUES(8,'other_fixture','Other','fake_hash')")
	exec(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,7,'34ce1163-cc34-4e7a-bac1-1af5c8726e00','Uninitialized','WEB',NULL),
		(21,7,'34ce1163-cc34-4e7a-bac1-1af5c8726e01','Revoked','WEB',UTC_TIMESTAMP(6)),
		(22,7,'34ce1163-cc34-4e7a-bac1-1af5c8726e02','Zero generation','WEB',NULL)`)
	first := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	exec(`INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at) VALUES
		(19,7,?,UTC_TIMESTAMP(6)),(21,7,?,UTC_TIMESTAMP(6)),(22,7,UNHEX(REPEAT('00',16)),UTC_TIMESTAMP(6))`, first[:], first[:])
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 验证归属、状态、取消和读取入口的错误边界。
	for _, testCase := range []struct {
		name             string
		userID, deviceID uint64
		want             error
	}{
		{"different account", 8, 19, domain.ErrDeviceNotFound},
		{"missing device", 7, 99, domain.ErrDeviceNotFound},
		{"missing sync record", 7, 20, domain.ErrSyncDeviceNotFound},
		{"revoked with sync record", 7, 21, domain.ErrDeviceRevoked},
		{"zero generation", 7, 22, domain.ErrInvalidSyncDeviceData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if result, err := store.FindSyncDeviceGeneration(ctx, testCase.userID, testCase.deviceID); result != (domain.SyncCursorGeneration{}) || !errors.Is(err, testCase.want) {
				t.Fatalf("错误边界不符，错误=%v，预期=%v", err, testCase.want)
			}
		})
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if result, err := store.FindSyncDeviceGeneration(canceled, 7, 19); result != (domain.SyncCursorGeneration{}) || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后不应返回代次，错误=%v", err)
	}
	if err := store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
		if result, err := reader.FindSyncDeviceGeneration(readContext, 7, 19); result != (domain.SyncCursorGeneration{}) || err == nil {
			t.Fatal("事务内入口不应交付可能过时的代次")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 3. 使用第二条连接修改代次，读取在提交前后分别交付各自已提交的值。
	assertGeneration := func(queryContext context.Context, expected domain.SyncCursorGeneration) {
		t.Helper()
		if result, err := store.FindSyncDeviceGeneration(queryContext, 7, 19); result != expected || err != nil {
			t.Fatalf("已提交代次不符，实际=%x，预期=%x，错误=%v", result, expected, err)
		}
	}
	assertGeneration(ctx, first)
	committed := first
	committed[15] ^= 1
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, "UPDATE sync_devices SET cursor_generation=?,status='ACTIVE',ack_sequence=17 WHERE device_id=19", committed[:]); err != nil {
		t.Fatal(err)
	}
	readContext, release := context.WithTimeout(ctx, 2*time.Second)
	defer release()
	assertGeneration(readContext, first)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	var ack uint64
	var status string
	var before, after time.Time
	if err := pool.QueryRowContext(ctx, "SELECT last_seen_at FROM sync_devices WHERE device_id=19").Scan(&before); err != nil {
		t.Fatal(err)
	}
	assertGeneration(ctx, committed)
	if err := pool.QueryRowContext(ctx, "SELECT ack_sequence,status,last_seen_at FROM sync_devices WHERE device_id=19").Scan(&ack, &status, &after); err != nil ||
		ack != 17 || status != "ACTIVE" || !after.Equal(before) {
		t.Fatalf("只读查询改变了同步状态、确认位置或活动时间，错误=%v", err)
	}
}
