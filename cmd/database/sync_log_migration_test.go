package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/migrations"
)

const syncMigrationLedger = `SELECT GROUP_CONCAT(JSON_ARRAY(version,name,checksum,dirty,started_at,applied_at)
	ORDER BY version SEPARATOR '\n') FROM schema_migrations`

func TestSyncLogMigrationIntegration(t *testing.T) {
	catalog := migrations.Ordered()
	t.Run("empty schema", func(t *testing.T) {
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		verifySyncMigrationAndRerun(t, connection, databaseName, catalog)
		t.Log("空库依次应用 1 至当前版本，启动检查通过，重跑保持原记录")
	})

	t.Run("version 2 data and sync constraints", func(t *testing.T) {
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")

		// 1. 在合法 v2 结构上写入合成 B4 记录并保存升级前的业务和迁移快照。
		if err := migrate(t.Context(), connection, catalog[:2]); err != nil {
			t.Fatal(err)
		}
		seedSyncMigrationBusinessData(t, connection)
		before := readSyncMigrationValue(t, connection, syncMigrationBusinessSnapshot)
		oldLedger := readSyncMigrationValue(t, connection, syncMigrationLedger)

		// 2. 由正式迁移入口升级；启动检查与重跑均保留已有业务和迁移记录。
		verifySyncMigrationAndRerun(t, connection, databaseName, catalog)
		if readSyncMigrationValue(t, connection, syncMigrationBusinessSnapshot) != before ||
			readSyncMigrationValue(t, connection, syncMigrationLedger+" WHERE version<=2") != oldLedger {
			t.Fatal("版本 3 改变了原账号、文件、上传任务、回执、配额或已应用历史")
		}
		t.Log("从 v2 升级至当前版本，B4 文件、任务、两条回执、配额及旧迁移记录保持一致")

		// 3. 用真实索引和约束验证账号序号隔离、设备默认值及复合归属。
		verifySyncMigrationConstraints(t, connection)
	})

	t.Run("second DDL fails and leaves dirty history", func(t *testing.T) {
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		if err := migrate(t.Context(), connection, catalog[:2]); err != nil {
			t.Fatal(err)
		}
		// 第二张表的名字由合成夹具占用，触发原样执行的版本 3 SQL 在第二条 DDL 失败。
		executeSyncMigrationSQL(t, connection, "CREATE TABLE sync_devices (fixture_marker INT PRIMARY KEY)")
		executeSyncMigrationSQL(t, connection, "INSERT INTO sync_devices VALUES (7)")
		migrationError := run(databaseName, false)
		requireSyncMigrationMySQLError(t, migrationError, 1050)
		if !strings.Contains(migrationError.Error(), "migration 3 failed and is marked dirty") {
			t.Fatal(migrationError)
		}
		assertFixtureMigrationRecords(t, connection, catalog[:3], 2)
		dirtyLedger := readSyncMigrationValue(t, connection, syncMigrationLedger)
		if err := run(databaseName, false); err == nil || !strings.Contains(err.Error(), "migration 3 is dirty") {
			t.Fatal("重跑未拒绝部分执行的版本 3", err)
		}
		t.Setenv("CLOUDNEST_SCHEMA_TEST_DB", databaseName)
		if err := repository.CheckSchema(t.Context(), verificationDB(t)); err == nil ||
			!strings.Contains(err.Error(), "迁移版本 3 的 dirty=true") {
			t.Fatal("服务启动检查未拒绝部分执行的版本 3", err)
		}
		if readSyncMigrationValue(t, connection, "SELECT COUNT(*) FROM change_events") != "0" ||
			readSyncMigrationValue(t, connection, "SELECT fixture_marker FROM sync_devices") != "7" ||
			readSyncMigrationValue(t, connection, syncMigrationLedger) != dirtyLedger {
			t.Fatal("失败、重跑或启动检查改变了部分 DDL 现场")
		}
		t.Log("第二条 DDL 返回 1050；第一张表已建，版本 3 保持 dirty，重跑与启动均拒绝")
	})
}

const syncMigrationBusinessSnapshot = `SELECT GROUP_CONCAT(JSON_ARRAY(u.id,u.username,u.change_seq,u.storage_quota,u.used_storage,u.reserved_storage,
	d.id,e.id,e.parent_id,e.name,e.revision,o.id,o.object_key,HEX(o.verified_sha256),t.id,t.status,t.staged_object_id,
	t.storage_upload_id,t.part_size,t.part_count,t.expires_at,t.reserved_bytes,p.part_number,p.size,p.etag,
	p.checksum_algorithm,p.checksum_value,p.observed_at) ORDER BY p.part_number SEPARATOR '\n')
	FROM users u JOIN user_devices d ON d.user_id=u.id JOIN file_entries e ON e.user_id=u.id
	JOIN stored_objects o ON o.id=e.object_id JOIN upload_tasks t ON t.user_id=u.id
	JOIN upload_parts p ON p.task_id=t.id WHERE u.id=7 AND d.id=11 AND e.id=32 AND t.id=41`

func seedSyncMigrationBusinessData(t *testing.T, connection *sql.Conn) {
	t.Helper()
	executeSyncMigrationSQL(t, connection, `INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage)
		VALUES(7,'sync_fixture_a','Fixture A','fixture-hash',7,16777217),(8,'sync_fixture_b','Fixture B','fixture-hash',0,0)`)
	executeSyncMigrationSQL(t, connection, `INSERT INTO user_devices(id,user_id,installation_id,name,platform)
		VALUES(11,7,'00000000-0000-4000-8000-000000000001','Fixture A','WINDOWS'),
		      (12,8,'00000000-0000-4000-8000-000000000002','Fixture B','WINDOWS')`)
	executeSyncMigrationSQL(t, connection, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
		VALUES(21,7,'local_minio','fixture','published',7,'READY',UNHEX(SHA2('fixture',256)),UTC_TIMESTAMP(6)),
		      (22,7,'local_minio','fixture','multipart',16777217,'STAGING',NULL,NULL)`)
	executeSyncMigrationSQL(t, connection, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(31,7,NULL,'',X'','DIR')")
	executeSyncMigrationSQL(t, connection, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES(32,7,31,'b4.txt','b4.txt','FILE','DOCUMENT',21,7)`)
	executeSyncMigrationSQL(t, connection, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,
		upload_mode,storage_upload_id,expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(41,7,11,22,31,'pending.bin','MULTIPART','fixture-upload',UNHEX(SHA2('fixture-multipart',256)),
		16777217,16777216,2,'COMPLETING',16777217,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
	executeSyncMigrationSQL(t, connection, `INSERT INTO upload_parts(task_id,part_number,etag,size,checksum_algorithm,checksum_value,observed_at)
		VALUES(41,1,'fixture-etag-one',16777216,'SHA256','YWJjZA==','2026-11-21 00:00:00.123456'),
		      (41,2,'fixture-etag-two',1,'SHA256','ZGVmZw==','2026-11-21 00:00:00.123457')`)
}

func verifySyncMigrationAndRerun(t *testing.T, connection *sql.Conn, databaseName string, catalog []migrations.Migration) {
	t.Helper()
	if err := run(databaseName, false); err != nil {
		t.Fatal(err)
	}
	assertFixtureMigrationRecords(t, connection, catalog, len(catalog))
	if readSyncMigrationValue(t, connection, "SELECT (SELECT COUNT(*) FROM change_events)+(SELECT COUNT(*) FROM sync_devices)") != "0" {
		t.Fatal("版本 3 初始化应留下两个空表")
	}
	ledger := readSyncMigrationValue(t, connection, syncMigrationLedger)
	if err := run(databaseName, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_SCHEMA_TEST_DB", databaseName)
	if err := repository.CheckSchema(t.Context(), verificationDB(t)); err != nil {
		t.Fatal(err)
	}
	if readSyncMigrationValue(t, connection, syncMigrationLedger) != ledger {
		t.Fatal("重跑或启动检查改写了迁移记录")
	}
}

func verifySyncMigrationConstraints(t *testing.T, connection *sql.Conn) {
	t.Helper()
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(?,?,?,?,?,'{}')"
	executeSyncMigrationSQL(t, connection, insertEvent, 7, 1, 32, 1, "ENTRY_CREATED")
	executeSyncMigrationSQL(t, connection, insertEvent, 8, 1, 999, 1, "ENTRY_TRASHED")
	for _, sample := range []struct {
		userID, sequence, entryID, revision int
		eventType                           string
		code                                uint16
	}{
		{7, 1, 32, 1, "ENTRY_CREATED", 1062},
		{7, 0, 32, 1, "ENTRY_CREATED", 3819},
		{7, 2, 0, 1, "ENTRY_CREATED", 3819},
		{7, 2, 32, 0, "ENTRY_CREATED", 3819},
		{7, 2, 32, 1, "", 3819},
		{999, 1, 32, 1, "ENTRY_CREATED", 1452},
	} {
		_, err := connection.ExecContext(t.Context(), insertEvent, sample.userID, sample.sequence, sample.entryID, sample.revision, sample.eventType)
		requireSyncMigrationMySQLError(t, err, sample.code)
	}

	const insertDevice = `INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at)
		VALUES(?,?,UNHEX('000102030405060708090A0B0C0D0E0F'),'2026-11-21 00:00:00.123456')`
	_, err := connection.ExecContext(t.Context(), insertDevice, 12, 7)
	requireSyncMigrationMySQLError(t, err, 1452)
	executeSyncMigrationSQL(t, connection, insertDevice, 11, 7)
	executeSyncMigrationSQL(t, connection, insertDevice, 12, 8)
	_, err = connection.ExecContext(t.Context(), insertDevice, 11, 7)
	requireSyncMigrationMySQLError(t, err, 1062)
	for _, status := range []string{"UNKNOWN", "active", "ACTIVE "} {
		_, err := connection.ExecContext(t.Context(), "UPDATE sync_devices SET status=? WHERE device_id=11", status)
		requireSyncMigrationMySQLError(t, err, 3819)
	}
	if got := readSyncMigrationValue(t, connection, `SELECT CONCAT_WS('|',status,ack_sequence,HEX(cursor_generation),last_seen_at)
		FROM sync_devices WHERE device_id=11`); got != "RESET_REQUIRED|0|000102030405060708090A0B0C0D0E0F|2026-11-21 00:00:00.123456" {
		t.Fatal("同步设备默认值、代次或微秒时间不符", got)
	}
	executeSyncMigrationSQL(t, connection, "UPDATE sync_devices SET status='ACTIVE',ack_sequence=1 WHERE device_id=11")
	if readSyncMigrationValue(t, connection, "SELECT COUNT(*) FROM change_events") != "2" ||
		readSyncMigrationValue(t, connection, "SELECT COUNT(*) FROM sync_devices WHERE device_id=11 AND status='ACTIVE' AND ack_sequence=1") != "1" {
		t.Fatal("变更事件隔离或 ACTIVE 状态不符")
	}
	t.Log("校验通过：账号序号、非零标识和版本、设备归属、状态、默认确认与 UTC 微秒")
}

func executeSyncMigrationSQL(t *testing.T, connection *sql.Conn, statement string, arguments ...any) {
	t.Helper()
	if _, err := connection.ExecContext(t.Context(), statement, arguments...); err != nil {
		t.Fatal(err)
	}
}

func readSyncMigrationValue(t *testing.T, connection *sql.Conn, query string) string {
	t.Helper()
	var value string
	if err := connection.QueryRowContext(t.Context(), query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func requireSyncMigrationMySQLError(t *testing.T, err error, number uint16) {
	t.Helper()
	var mysqlError *mysql.MySQLError
	if !errors.As(err, &mysqlError) || mysqlError.Number != number {
		t.Fatalf("预期 MySQL %d，实际 %v", number, err)
	}
}
