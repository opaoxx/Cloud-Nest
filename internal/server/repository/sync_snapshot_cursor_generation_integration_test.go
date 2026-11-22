package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestSyncSnapshotCursorGenerationIntegration(t *testing.T) {
	// 1. 用正式迁移建立合成设备，写入含零字节及高位字节的 16 字节代次。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
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
	stored := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	if _, err := pool.ExecContext(ctx, "INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at) VALUES(19,7,?,UTC_TIMESTAMP(6))", stored[:]); err != nil {
		t.Fatal("写入代次夹具失败", err)
	}
	var generation []byte
	if err := pool.QueryRowContext(ctx, "SELECT cursor_generation FROM sync_devices WHERE user_id=7 AND device_id=19").Scan(&generation); err != nil || !bytes.Equal(generation, stored[:]) {
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
		Generation: domain.SyncCursorGeneration(generation),
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
