package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestServerBackupRehearsalIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后执行阶段 1 隔离备份恢复演练")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 停写源端，核对同轮备份，再恢复到独立目标并登录合成账号。
	running := startRestoredBackupTarget(t, ctx)

	// 2. 等待既有取消流程结算预占，再从同一个目标下载三份样本。
	finished := cancelRestoredBackupUpload(t, ctx, running)
	t.Logf("未发送任务 %d 已取消，3 B 预占已返还：UTC %s", finished.ID, finished.CompletedAt.UTC().Format(time.RFC3339Nano))
	verifyRestoredBackupActiveDownloads(t, ctx, running)
	verifyRestoredBackupTrashDownload(t, ctx, running)

	// 3. 从目标库独立重算已用和预占，核对公开配额所依据的账目。
	var used, reserved, quota, fileBytes, taskBytes uint64
	if err := running.fixture.database.pool.QueryRowContext(ctx, `SELECT u.used_storage, u.reserved_storage, u.storage_quota,
		COALESCE((SELECT SUM(e.file_size) FROM file_entries e WHERE e.user_id=u.id AND e.entry_type='FILE' AND e.status<>'PURGED'), 0),
		COALESCE((SELECT SUM(t.reserved_bytes) FROM upload_tasks t WHERE t.user_id=u.id), 0)
		FROM users u WHERE u.id=?`, running.pending.UserID).Scan(&used, &reserved, &quota, &fileBytes, &taskBytes); err != nil {
		t.Fatal("目标库配额复算失败", err)
	}
	if used != fileBytes || reserved != taskBytes || used != 10 || reserved != 0 || used+reserved > quota {
		t.Fatalf("目标配额不符，库记录 %d/%d，按文件和任务复算 %d/%d，配额上限 %d", used, reserved, fileBytes, taskBytes, quota)
	}
	acceptedAt := time.Now().UTC()
	logs, err := running.stop()
	if err != nil {
		t.Fatal("隔离目标后端关停失败", err)
	}
	verifyBackupTargetCancellationRound(t, logs)
	t.Logf("隔离目标业务验收完成：UTC %s，数据库复算已用 %d B、预占 %d B，取消轮次汇报一致",
		acceptedAt.Format(time.RFC3339Nano), fileBytes, taskBytes)
}
