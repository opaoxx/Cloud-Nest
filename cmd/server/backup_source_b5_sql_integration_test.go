package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestServerBackupSourceB5SQLIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 隔离源端 SQL 导出")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 在同一源端建立同步基线和两版文件。
	source := prepareBackupSource(t, ctx)
	verifyBackupSourceSyncBaseline(t, ctx, source)
	fixture := prepareReplacementPublicationFixtureFromSource(t, ctx, source)
	verifyBackupSourceFileVersions(t, ctx, fixture)
	stopAndExportBackupSourceB5SQL(t, ctx, fixture, t.TempDir())
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}

// backupSourceSQLRowPrefix 保存停写后用于比对 SQL 单行数据的字段前缀。
type backupSourceSQLRowPrefix struct {
	table  string
	values string
}

func stopAndExportBackupSourceB5SQL(t *testing.T, ctx context.Context, fixture replacementPublicationFixture, backupDir string) []byte {
	t.Helper()
	// 2. 等 HTTP 和启动的后台任务退出，再读取停写基线。
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("B5 隔离源端关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	stoppedAt := time.Now().UTC()
	rows := inspectB5BackupSourceRows(t, ctx, fixture)

	// 3. 导出完整 SQL，比对表、关键记录和容器内外 SHA-256。
	tableRows := map[string]int{
		"users": 1, "user_devices": 1, "auth_sessions": 1, "stored_objects": 2,
		"file_entries": 4, "user_entry_preferences": 0, "upload_tasks": 2, "idempotency_records": 5,
		"upload_parts": 0, "change_events": 4, "sync_devices": 1, "file_versions": 1, "schema_migrations": len(migrations.Ordered()),
	}
	for table, expected := range tableRows {
		var count int
		if err := fixture.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil || count != expected {
			t.Fatalf("停写基线表 %s 有 %d 行，预期 %d，错误 %v", table, count, expected, err)
		}
	}
	dump := exportBackupSourceSQL(t, ctx, fixture.backupSourceFixture, backupDir)
	verifyBackupSourceSQL(t, dump, tableRows)
	dumpLines := strings.Split(string(dump), "\n")
	for _, row := range rows {
		matches := 0
		insert := "INSERT INTO `" + row.table + "` "
		for _, line := range dumpLines {
			if !strings.HasPrefix(line, insert) {
				continue
			}
			_, values, found := strings.Cut(line, " VALUES (")
			if found && strings.HasPrefix(values, row.values) {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("SQL 快照中 %s 的关键行匹配 %d 次，预期一次", row.table, matches)
		}
	}
	t.Logf("B5 同点导出，停写 UTC %s，SQL %d B，容器内外 SHA-256 %x，%d 张表逐表一致，确认位置 2，上界 4，历史 1 条",
		stoppedAt.Format(time.RFC3339Nano), len(dump), sha256.Sum256(dump), len(tableRows))
	return dump
}

func verifyBackupB5ServiceStopped(t *testing.T, baseURL, logs string) {
	t.Helper()
	// 1. 核对既有 HTTP 和 worker 关停，再核对 B5 扩展 worker 的退出顺序。
	verifyBackupSourceStopped(t, baseURL, logs)
	storageClosed := strings.Index(logs, "MinIO 空闲连接已释放")
	for _, worker := range []string{"分片合并", "分片校验", "分片上传取消", "分片上传到期", "历史到期结算", "同步维护"} {
		workerStopped := strings.Index(logs, worker+" worker 已停止")
		if workerStopped < 0 || workerStopped >= storageClosed {
			t.Fatal("B5 worker 未在依赖关闭前退出", worker)
		}
	}
}

func inspectB5BackupSourceRows(t *testing.T, ctx context.Context, fixture replacementPublicationFixture) []backupSourceSQLRowPrefix {
	t.Helper()
	deviceID := backupFixtureID(t, fixture.login.Device.ID)
	var device domain.SyncDeviceState
	var high, pending uint64
	var generation []byte
	err := fixture.pool.QueryRowContext(ctx, `SELECT d.status,d.ack_sequence,d.cursor_generation,u.change_seq,
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=u.id AND status NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED'))
		FROM sync_devices AS d JOIN users AS u ON u.id=d.user_id WHERE d.user_id=? AND d.device_id=?`, fixture.userID, deviceID).
		Scan(&device.Status, &device.AckSequence, &generation, &high, &pending)
	if err != nil || len(generation) != len(device.Generation) || device.Status != domain.SyncDeviceStatusActive ||
		device.AckSequence != 2 || high != 4 || pending != 0 {
		t.Fatal("B5 停写时的设备位置、账号上界或上传状态不符", err)
	}
	copy(device.Generation[:], generation)
	if !device.IsValid() {
		t.Fatal("停写时设备代次无效")
	}
	rows := []backupSourceSQLRowPrefix{{"sync_devices",
		fmt.Sprintf("%d,%d,%d,'%s',0x%X,", deviceID, fixture.userID, device.AckSequence, device.Status, generation)}}
	for index, eventType := range []string{"DIRECTORY_CREATED", "DIRECTORY_CREATED",
		domain.FilePublishedEventType, domain.FileContentReplacedEventType} {
		sequence := uint64(index + 1)
		var entryID, revision uint64
		var storedType string
		if err := fixture.pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type FROM change_events WHERE user_id=? AND `sequence`=?",
			fixture.userID, sequence).Scan(&entryID, &revision, &storedType); err != nil || storedType != eventType {
			t.Fatalf("停写日志序号 %d 的事件不符，错误 %v", sequence, err)
		}
		rows = append(rows, backupSourceSQLRowPrefix{"change_events",
			fmt.Sprintf("%d,%d,%d,%d,'%s',", fixture.userID, sequence, entryID, revision, storedType)})
	}

	entryID := fixture.target.ID
	var history domain.FileVersion
	err = fixture.pool.QueryRowContext(ctx, `SELECT id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until
		FROM file_versions WHERE user_id=? AND entry_id=?`, fixture.userID, entryID).
		Scan(&history.ID, &history.UserID, &history.EntryID, &history.Revision, &history.ObjectID, &history.FileSize, &history.CreatedAt, &history.RetainedUntil)
	if err != nil || !history.IsValid() || history.ObjectID != fixture.originalObject.ID || history.Revision != fixture.target.Revision {
		t.Fatal("停写后的历史引用不符", err)
	}
	rows = append(rows, backupSourceSQLRowPrefix{"file_versions",
		fmt.Sprintf("%d,%d,%d,%d,%d,%d,", history.ID, history.UserID, history.EntryID, history.Revision, history.ObjectID, history.FileSize)})
	current, err := fixture.store.FindUserFileEntryContent(ctx, fixture.userID, entryID)
	if err != nil || current == nil || current.ObjectID == nil || *current.ObjectID == history.ObjectID {
		t.Fatal("停写后的当前对象引用不符", err)
	}
	for _, objectID := range []uint64{history.ObjectID, *current.ObjectID} {
		object, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, objectID)
		if err != nil || object == nil || object.Status != domain.StoredObjectStatusReady || object.StorageVersionID == nil ||
			object.VerifiedSHA256 == nil || object.Bucket != fixture.bucket.name {
			t.Fatal("停写时对象位置或内容摘要不符", err)
		}
		rows = append(rows, backupSourceSQLRowPrefix{"stored_objects",
			fmt.Sprintf("%d,%d,'%s','%s','%s','%s',%d,0x%X,'%s',", object.ID, object.OwnerUserID, object.Provider,
				object.Bucket, object.ObjectKey, *object.StorageVersionID, object.FileSize, object.VerifiedSHA256[:], object.Status)})
	}
	return rows
}
