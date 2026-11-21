package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/migrations"
)

func TestServerBackupSourceDumpIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后导出隔离备份源端")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	verifyBackupSourceBaseline(t, ctx, fixture)

	// 1. 等待服务优雅退出，确认后台任务和数据库连接已经结束。
	startedAt := time.Now().UTC()
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal(err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	stoppedAt := time.Now().UTC()
	t.Logf("停写确认：UTC %s 至 %s，HTTP 及六个 worker 已停止，连接池在 worker 后关闭",
		startedAt.Format(time.RFC3339Nano), stoppedAt.Format(time.RFC3339Nano))

	// 2. 从停止写入的专用库记录基线，再导出全部结构和数据。
	tableRows, migrationChecksum := inspectBackupSourceForDump(t, ctx, fixture)
	for _, container := range []struct{ name, id string }{{"MySQL", fixture.mysqlContainer}, {"MinIO", fixture.minioContainer}} {
		image, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", "{{.Image}}", container.id).Output()
		if err != nil {
			t.Fatal("无法核对隔离容器使用的镜像", container.name, err)
		}
		t.Logf("%s 容器 %s，镜像 %s", container.name, container.id, strings.TrimSpace(string(image)))
	}
	dump := exportBackupSourceSQL(t, ctx, fixture)

	// 3. 核对导出文件中的表、逐行记录和迁移摘要，保留可复验的日志。
	verifyBackupSourceSQL(t, dump, tableRows, migrationChecksum)
	t.Logf("SQL 快照 %d B，SHA-256 %x，结构共 %d 张表，数据行数与停写基线逐表一致", len(dump), sha256.Sum256(dump), len(tableRows))
}

func verifyBackupSourceStopped(t *testing.T, baseURL, logs string) {
	t.Helper()
	httpStopped := strings.Index(logs, "HTTP 服务已停止")
	storageClosed := strings.Index(logs, "MinIO 空闲连接已释放")
	databaseClosed := strings.Index(logs, "MySQL 连接池已关闭")
	if httpStopped < 0 || storageClosed <= httpStopped || databaseClosed <= storageClosed {
		t.Fatal("源端 HTTP 和连接资源未按顺序关闭")
	}
	for _, worker := range []string{"上传校验", "上传过期", "上传取消", "永久清理", "对象回收", "回收站到期标记"} {
		workerStopped := strings.Index(logs, worker+" worker 已停止")
		if workerStopped < 0 || workerStopped >= storageClosed {
			t.Fatal("源端 worker 未在依赖关闭前退出", worker)
		}
	}
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(baseURL, "http://"), time.Second)
	if err == nil {
		_ = connection.Close()
		t.Fatal("停写后源端 HTTP 端口仍然可以连接")
	}
}

func inspectBackupSourceForDump(t *testing.T, ctx context.Context, fixture backupSourceFixture) (map[string]int, string) {
	t.Helper()
	// 1. 确认停写后仍然只有一个未发送任务，并且没有待删除或到期清理的样本。
	var used, reserved, pending, unsent, purging, deleting int
	if err := fixture.pool.QueryRowContext(ctx, `SELECT u.used_storage,u.reserved_storage,
		(SELECT COUNT(*) FROM upload_tasks WHERE status NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED')),
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=u.id AND status='UPLOADING' AND upload_mode='SINGLE'
			AND lease_owner IS NULL AND lease_until IS NULL AND reserved_bytes=3 AND expires_at>UTC_TIMESTAMP(6)),
		(SELECT COUNT(*) FROM file_entries WHERE status='PURGING' OR (status='TRASHED' AND deleted_at<UTC_TIMESTAMP(6)-INTERVAL 30 DAY)),
		(SELECT COUNT(*) FROM stored_objects WHERE status='DELETING' OR gc_after<=UTC_TIMESTAMP(6))
		FROM users u WHERE u.id=?`, backupFixtureID(t, fixture.login.User.ID)).
		Scan(&used, &reserved, &pending, &unsent, &purging, &deleting); err != nil {
		t.Fatal(err)
	}
	if used != 10 || reserved != 3 || pending != 1 || unsent != 1 || purging != 0 || deleting != 0 {
		t.Fatalf("停写基线不符，已用 %d，预占 %d，待处理 %d，未发送 %d，目录清理 %d，对象回收 %d",
			used, reserved, pending, unsent, purging, deleting)
	}

	// 2. 记录正式迁移状态和九张表的行数，以便独立核对导出结构和数据。
	var version int
	var name, checksum string
	var dirty bool
	var appliedAt time.Time
	if err := fixture.pool.QueryRowContext(ctx, "SELECT version,name,checksum,dirty,applied_at FROM schema_migrations").
		Scan(&version, &name, &checksum, &dirty, &appliedAt); err != nil {
		t.Fatal(err)
	}
	if version != 1 || name != "000001_initial.up.sql" || dirty || appliedAt.IsZero() ||
		checksum != fmt.Sprintf("%x", sha256.Sum256([]byte(migrations.InitialSQL))) {
		t.Fatal("源端迁移记录与当前程序不兼容")
	}
	t.Logf("迁移记录：版本 %d，文件 %s，SHA-256 %s，dirty=%t，应用时间 %s",
		version, name, checksum, dirty, appliedAt.Format(time.RFC3339Nano))
	counts := map[string]int{
		"users": 1, "user_devices": 1, "auth_sessions": 1, "stored_objects": 4,
		"file_entries": 5, "user_entry_preferences": 0, "upload_tasks": 4, "idempotency_records": 6,
		"schema_migrations": 1,
	}
	for table, expected := range counts {
		var rows int
		if err := fixture.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&rows); err != nil || rows != expected {
			t.Fatalf("停写后的表 %s 行数 %d，预期 %d，错误 %v", table, rows, expected, err)
		}
	}
	return counts, checksum
}

func exportBackupSourceSQL(t *testing.T, ctx context.Context, fixture backupSourceFixture) []byte {
	t.Helper()
	const containerPath = "/tmp/cloudnest-backup.sql"
	export := exec.CommandContext(ctx, "docker", "exec", "--env", "MYSQL_PWD", fixture.mysqlContainer,
		"mysqldump", "--user=root", "--single-transaction", "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF",
		"--skip-add-drop-table", "--skip-extended-insert", "--result-file="+containerPath, fixture.databaseName)
	export.Env = append(export.Environ(), "MYSQL_PWD="+os.Getenv("CLOUDNEST_DB_PASSWORD"))
	if output, err := export.CombinedOutput(); err != nil {
		t.Fatalf("隔离源端导出失败，%v\n%s", err, output)
	}
	localPath := filepath.Join(t.TempDir(), "database.sql")
	if output, err := exec.CommandContext(ctx, "docker", "cp", fixture.mysqlContainer+":"+containerPath, localPath).CombinedOutput(); err != nil {
		t.Fatalf("复制隔离 SQL 快照失败，%v\n%s", err, output)
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal("读回临时 SQL 快照失败", err)
	}
	containerSum, err := exec.CommandContext(ctx, "docker", "exec", fixture.mysqlContainer, "sha256sum", containerPath).Output()
	fields := strings.Fields(string(containerSum))
	if err != nil || len(fields) != 2 || fields[1] != containerPath || fields[0] != fmt.Sprintf("%x", sha256.Sum256(local)) {
		t.Fatal("容器内外的 SQL 快照 SHA-256 不一致", err)
	}
	return local
}

func verifyBackupSourceSQL(t *testing.T, dump []byte, tableRows map[string]int, migrationChecksum string) {
	t.Helper()
	if !bytes.Contains(dump, []byte("\n-- Dump completed on ")) || bytes.Count(dump, []byte("\nCREATE TABLE ")) != len(tableRows) ||
		bytes.Contains(dump, []byte("\nUSE ")) || bytes.Contains(dump, []byte("\nDROP TABLE ")) {
		t.Fatal("SQL 快照的完成标记、表数量或导入范围不符")
	}
	for table, expected := range tableRows {
		create := []byte("\nCREATE TABLE `" + table + "` (")
		// 单行 INSERT 可直接跟 VALUES，也可先列出可写列，如含生成列的对象表。
		insert := "\nINSERT INTO `" + table + "` "
		rows := bytes.Count(dump, []byte(insert+"VALUES ")) + bytes.Count(dump, []byte(insert+"("))
		if bytes.Count(dump, create) != 1 || rows != expected {
			t.Fatalf("SQL 快照中的表 %s 导出 %d 行，预期 %d 行", table, rows, expected)
		}
	}
	migration := []byte("\nINSERT INTO `schema_migrations` VALUES (1,'000001_initial.up.sql','" + migrationChecksum + "',0,")
	if !bytes.Contains(dump, migration) {
		t.Fatal("导出文件的迁移记录与停写基线不符")
	}
}
