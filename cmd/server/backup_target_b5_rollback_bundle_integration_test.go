package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/migrations"
)

func TestServerBackupTargetB5RollbackBundleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 切换后回退备份")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	exportBackupTargetB5RollbackBundle(t, ctx, sample, t.TempDir())
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}

func exportBackupTargetB5RollbackBundle(t *testing.T, ctx context.Context, sample backupTargetB5RollbackSample, backupDir string) backupSourceManifest {
	t.Helper()
	// 1. 从已完成关停的隔离目标库导出 SQL，并核对表和关键记录。
	target := sample.fixture.database
	bucket := sample.fixture.targetBucket
	var databaseName string
	if err := target.pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil ||
		databaseName != target.name || databaseName == sample.fixture.source.databaseName ||
		target.containerID == sample.fixture.source.mysqlContainer || os.Getenv("CLOUDNEST_DB_NAME") != databaseName ||
		os.Getenv("CLOUDNEST_S3_BUCKET") != bucket.name || bucket.name == sample.fixture.source.bucket.name {
		t.Fatal("导出未绑定到本轮切换后独占库和桶", err)
	}
	tableRows := map[string]int{
		"users": 1, "user_devices": 2, "auth_sessions": 2, "stored_objects": 3,
		"file_entries": 4, "user_entry_preferences": 0, "upload_tasks": 3, "idempotency_records": 6,
		"upload_parts": 0, "change_events": 5, "sync_devices": 1, "file_versions": 2, "schema_migrations": len(migrations.Ordered()),
	}
	for table, expected := range tableRows {
		var count int
		if err := target.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil || count != expected {
			t.Fatalf("停写目标的表 %s 行数 %d，预期 %d，错误 %v", table, count, expected, err)
		}
	}
	dump := exportBackupSourceSQL(t, ctx, target.containerID, target.name, backupDir)
	verifyBackupSourceSQL(t, dump, tableRows)
	verifyBackupTargetB5RollbackSQLRows(t, dump, sample)

	// 2. 将 SQL 摘要写入清单，按三个指定对象版本保存正文。
	manifest, writtenManifest := writeBackupSourceManifest(t, ctx, target.pool, bucket.name, backupDir, dump)
	expected := sample.manifest
	expected.DatabaseSHA256 = fmt.Sprintf("%x", sha256.Sum256(dump))
	if !reflect.DeepEqual(manifest, expected) {
		t.Fatal("导出清单的目录、当前引用、历史引用或对象定位与停写记录不符")
	}
	root := openBackupSourceBundleRoot(t, backupDir)
	saveAndVerifyBackupSourcePayloads(t, ctx, bucket, root, manifest.Objects)

	// 3. 从备份目录读回 SQL 和清单，核对摘要绑定及停写窗口的数据。
	savedSQL, err := root.ReadFile("database.sql")
	if err != nil || !bytes.Equal(savedSQL, dump) {
		t.Fatal("读回的 SQL 与容器导出不一致", err)
	}
	savedManifest, err := root.ReadFile("manifest.json")
	if err != nil || !bytes.Equal(savedManifest, writtenManifest) {
		t.Fatal("读回的清单与写入时不一致", err)
	}
	var decoded backupSourceManifest
	if err := json.Unmarshal(savedManifest, &decoded); err != nil || !reflect.DeepEqual(decoded, expected) ||
		decoded.DatabaseSHA256 != fmt.Sprintf("%x", sha256.Sum256(savedSQL)) {
		t.Fatal("保存后的清单未绑定同目录的 SQL 和停写记录", err)
	}
	if after := readBackupB5DatabaseState(t, ctx, target.pool, decoded.FileReferences[0].UserID); !reflect.DeepEqual(after, sample.state) {
		t.Fatal("导出期间停写目标的配额、日志或设备记录发生变化")
	}
	t.Logf("B5 切换后备份，SQL %d B，SHA-256 %s，%d 张表，清单 SHA-256 %x，当前 v3 和历史 v1/v2 三份正文合计 %d B，日志 1～%d",
		len(savedSQL), decoded.DatabaseSHA256, len(tableRows), sha256.Sum256(savedManifest), sample.state.used, sample.state.high)
	return decoded
}

func verifyBackupTargetB5RollbackSQLRows(t *testing.T, dump []byte, sample backupTargetB5RollbackSample) {
	t.Helper()
	// 1. 复核导出文件中设备代次和五条事件的关键字段。
	rows := make([]backupSourceSQLRowPrefix, 0, 11)
	for _, device := range sample.state.devices {
		rows = append(rows, backupSourceSQLRowPrefix{"sync_devices",
			fmt.Sprintf("%d,%d,%d,'%s',0x%X,", device.DeviceID, device.UserID, device.State.AckSequence, device.State.Status, device.State.Generation[:])})
	}
	current := sample.manifest.FileReferences[0]
	for _, event := range sample.state.events {
		rows = append(rows, backupSourceSQLRowPrefix{"change_events",
			fmt.Sprintf("%d,%d,%d,%d,'%s',", current.UserID, event.sequence, event.entryID, event.revision, event.eventType)})
	}

	// 2. 核对历史引用以及当前和历史正文对应的物理定位。
	for _, history := range sample.manifest.HistoryReferences {
		rows = append(rows, backupSourceSQLRowPrefix{"file_versions",
			fmt.Sprintf("%d,%d,%d,%d,%d,%d,", history.ID, history.UserID, history.EntryID, history.Revision, history.ObjectID, history.SizeBytes)})
	}
	for _, object := range sample.manifest.Objects {
		rows = append(rows, backupSourceSQLRowPrefix{"stored_objects",
			fmt.Sprintf("%d,%d,'%s','%s','%s','%s',%d,0x%s,'READY',", object.ID, object.UserID, object.Provider,
				object.Bucket, object.Key, *object.VersionID, object.SizeBytes, strings.ToUpper(object.SHA256))})
	}
	verifyBackupB5SQLRows(t, dump, rows)
}
