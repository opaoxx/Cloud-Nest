package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestFileVersionsMigrationIntegration(t *testing.T) {
	catalog := migrations.Ordered()
	t.Run("empty schema", func(t *testing.T) {
		// 1. 应用全量清单，并比较重跑前后的账本。
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		verifySyncMigrationAndRerun(t, connection, databaseName, catalog)
		verifyFileVersionsTable(t, connection)
		t.Log("空库依次应用 1、2、3、4，启动检查通过，历史表为空，重跑保持原记录")
	})

	t.Run("upgrade v3 and verify history constraints", func(t *testing.T) {
		// 1. 在合法 v3 库保留 B4 文件和任务，并添加 B5 变更事件及设备游标。
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		if err := migrate(t.Context(), connection, catalog[:3]); err != nil {
			t.Fatal(err)
		}
		seedSyncMigrationBusinessData(t, connection)
		executeSyncMigrationSQL(t, connection, "UPDATE users SET change_seq=1 WHERE id=7")
		executeSyncMigrationSQL(t, connection, "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(7,1,32,1,'FILE_PUBLISHED','{}')")
		executeSyncMigrationSQL(t, connection, `INSERT INTO sync_devices(device_id,user_id,ack_sequence,status,cursor_generation,last_seen_at)
			VALUES (11,7,1,'ACTIVE',UNHEX('000102030405060708090A0B0C0D0E0F'),'2026-11-21 00:00:00.123456')`)
		const syncSnapshot = "SELECT JSON_ARRAY(event.user_id,event.sequence,event.entry_id,event.revision,event.event_type,event.payload,event.created_at," +
			"device.device_id,device.ack_sequence,device.status,HEX(device.cursor_generation),device.last_seen_at,device.created_at,device.updated_at) " +
			"FROM change_events AS event JOIN sync_devices AS device ON device.user_id=event.user_id WHERE event.user_id=7"
		businessBefore := readSyncMigrationValue(t, connection, syncMigrationBusinessSnapshot)
		syncBefore := readSyncMigrationValue(t, connection, syncSnapshot)
		oldLedger := readSyncMigrationValue(t, connection, syncMigrationLedger)

		// 2. 运行正式迁移入口，核对只补版本 4，重跑和启动检查保持数据原值。
		if err := run(databaseName, false); err != nil {
			t.Fatal(err)
		}
		assertFixtureMigrationRecords(t, connection, catalog, 4)
		verifyFileVersionsTable(t, connection)
		ledger := readSyncMigrationValue(t, connection, syncMigrationLedger)
		if err := run(databaseName, false); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLOUDNEST_SCHEMA_TEST_DB", databaseName)
		if err := repository.CheckSchema(t.Context(), verificationDB(t)); err != nil {
			t.Fatal(err)
		}
		if readSyncMigrationValue(t, connection, syncMigrationLedger) != ledger ||
			readSyncMigrationValue(t, connection, syncMigrationLedger+" WHERE version<=3") != oldLedger ||
			readSyncMigrationValue(t, connection, syncMigrationBusinessSnapshot) != businessBefore ||
			readSyncMigrationValue(t, connection, syncSnapshot) != syncBefore {
			t.Fatal("版本 4、重跑或启动校验改写了既有账本、业务记录或同步状态")
		}
		t.Log("合法 v3 只补版本 4，账号、条目、对象、任务、回执、配额、事件和设备游标保持一致")

		// 3. 在升级后的结构里验证正例、外键和时间约束。
		verifyFileVersionConstraints(t, connection)
	})

	t.Run("DDL failure leaves version 4 dirty", func(t *testing.T) {
		// 1. 用冲突占位表让原始版本 4 SQL 失败。
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		if err := migrate(t.Context(), connection, catalog[:3]); err != nil {
			t.Fatal(err)
		}
		executeSyncMigrationSQL(t, connection, "CREATE TABLE file_versions (fixture_marker INT PRIMARY KEY)")
		executeSyncMigrationSQL(t, connection, "INSERT INTO file_versions VALUES (7)")
		migrationError := run(databaseName, false)
		requireSyncMigrationMySQLError(t, migrationError, 1050)
		if !strings.Contains(migrationError.Error(), "migration 4 failed and is marked dirty") {
			t.Fatal(migrationError)
		}
		assertFixtureMigrationRecords(t, connection, catalog, 3)
		dirtyLedger := readSyncMigrationValue(t, connection, syncMigrationLedger)

		// 2. 验证两条入口均拒绝 dirty，保留现场以便诊断。
		if err := run(databaseName, false); err == nil || !strings.Contains(err.Error(), "migration 4 is dirty") {
			t.Fatal("重跑未拒绝 dirty 的版本 4", err)
		}
		t.Setenv("CLOUDNEST_SCHEMA_TEST_DB", databaseName)
		if err := repository.CheckSchema(t.Context(), verificationDB(t)); err == nil ||
			!strings.Contains(err.Error(), "迁移版本 4 的 dirty=true") {
			t.Fatal("启动校验未拒绝 dirty 的版本 4", err)
		}
		if readSyncMigrationValue(t, connection, syncMigrationLedger) != dirtyLedger ||
			readSyncMigrationValue(t, connection, "SELECT fixture_marker FROM file_versions") != "7" {
			t.Fatal("失败重试改变了数据库现场")
		}
		t.Log("冲突 DDL 返回 1050，版本 4 保持 dirty，迁移重跑与启动均拒绝")
	})
}

func verifyFileVersionsTable(t *testing.T, connection *sql.Conn) {
	t.Helper()
	// 1. 核对引擎、列类型、精度和必填规则。
	if value := readSyncMigrationValue(t, connection, `SELECT CONCAT(ENGINE,'|',TABLE_COLLATION)
		FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='file_versions'`); value != "InnoDB|utf8mb4_0900_ai_ci" {
		t.Fatal("历史表引擎或字符排序规则不符", value)
	}
	columns := readSyncMigrationValue(t, connection, `SELECT GROUP_CONCAT(CONCAT_WS('|',COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,EXTRA)
		ORDER BY ORDINAL_POSITION SEPARATOR ',') FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='file_versions'`)
	wantColumns := strings.Join([]string{
		"id|bigint unsigned|NO|auto_increment", "user_id|bigint unsigned|NO|", "entry_id|bigint unsigned|NO|",
		"revision|bigint unsigned|NO|", "object_id|bigint unsigned|NO|", "file_size|bigint unsigned|NO|",
		"retained_until|datetime(6)|NO|", "created_at|datetime(6)|NO|",
	}, ",")
	if columns != wantColumns {
		t.Fatal("历史表列定义不符", columns)
	}

	// 2. 核对索引列顺序、唯一性和空表基线。
	indexes := readSyncMigrationValue(t, connection, `SELECT GROUP_CONCAT(CONCAT_WS(':',INDEX_NAME,NON_UNIQUE,column_list)
		ORDER BY INDEX_NAME SEPARATOR '|') FROM (
			SELECT INDEX_NAME,NON_UNIQUE,GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) AS column_list
			FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='file_versions'
			GROUP BY INDEX_NAME,NON_UNIQUE
		) AS file_version_indexes`)
	wantIndexes := strings.Join([]string{
		"idx_file_versions_expiry:1:retained_until,id", "idx_file_versions_owner_entry:1:user_id,entry_id",
		"idx_file_versions_owner_object:1:user_id,object_id", "PRIMARY:0:id", "uk_file_versions_entry_revision:0:entry_id,revision",
	}, "|")
	if indexes != wantIndexes || readSyncMigrationValue(t, connection, "SELECT COUNT(*) FROM file_versions") != "0" {
		t.Fatal("历史表索引或初始行数不符", indexes)
	}
}

func verifyFileVersionConstraints(t *testing.T, connection *sql.Conn) {
	t.Helper()
	// 1. 准备独立空内容对象及已逻辑清理的目录项，隔离其他外键的影响。
	executeSyncMigrationSQL(t, connection, `INSERT INTO stored_objects
		(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
		VALUES (23,7,'local_minio','fixture','empty-history-a',0,'READY',UNHEX(SHA2('',256)),UTC_TIMESTAMP(6)),
		       (24,8,'local_minio','fixture','empty-history-b',0,'READY',UNHEX(SHA2('',256)),UTC_TIMESTAMP(6))`)
	executeSyncMigrationSQL(t, connection, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (34,8,NULL,'',X'','DIR')")
	executeSyncMigrationSQL(t, connection, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,purged_at,revision)
		VALUES (33,7,31,'history-a.txt','history-a.txt','FILE','DOCUMENT','PURGED',UTC_TIMESTAMP(6),3),
		       (35,8,34,'history-b.txt','history-b.txt','FILE','DOCUMENT','PURGED',UTC_TIMESTAMP(6),2)`)

	// 2. 同账号可为相同对象保存多个版本引用，空文件可保留。
	const createdAt, retainedUntil = "2026-11-21 00:00:00.123456", "2026-12-21 00:00:00.123456"
	const insertVersion = "INSERT INTO file_versions(user_id,entry_id,revision,object_id,file_size,retained_until,created_at) VALUES(?,?,?,?,?,?,?)"
	executeSyncMigrationSQL(t, connection, insertVersion, 7, 33, 1, 23, 0, retainedUntil, createdAt)
	executeSyncMigrationSQL(t, connection, insertVersion, 7, 33, 2, 23, 0, retainedUntil, createdAt)
	executeSyncMigrationSQL(t, connection, insertVersion, 8, 35, 1, 24, 0, retainedUntil, createdAt)

	// 3. 核对跨账号引用、重复版本、非法时间与缺失字段的真实错误。
	for _, sample := range []struct {
		userID, entryID, revision, objectID int
		until                               string
		code                                uint16
	}{
		{7, 33, 1, 23, retainedUntil, 1062},
		{7, 35, 3, 23, retainedUntil, 1452},
		{7, 33, 3, 24, retainedUntil, 1452},
		{7, 33, 0, 23, retainedUntil, 3819},
		{7, 33, 3, 23, createdAt, 3819},
		{7, 33, 3, 23, "2026-11-21 00:00:00.123455", 3819},
	} {
		_, err := connection.ExecContext(t.Context(), insertVersion, sample.userID, sample.entryID, sample.revision, sample.objectID, 0, sample.until, createdAt)
		requireSyncMigrationMySQLError(t, err, sample.code)
	}
	_, err := connection.ExecContext(t.Context(), insertVersion, 7, 33, 3, 23, 0, nil, createdAt)
	requireSyncMigrationMySQLError(t, err, 1048)
	_, err = connection.ExecContext(t.Context(), "DELETE FROM file_entries WHERE user_id=7 AND id=33")
	requireSyncMigrationMySQLError(t, err, 1451)
	_, err = connection.ExecContext(t.Context(), "DELETE FROM stored_objects WHERE owner_user_id=7 AND id=23")
	requireSyncMigrationMySQLError(t, err, 1451)
	if value := readSyncMigrationValue(t, connection, `SELECT COUNT(*) FROM file_versions
		WHERE file_size=0 AND created_at='2026-11-21 00:00:00.123456' AND retained_until='2026-12-21 00:00:00.123456'`); value != "3" ||
		readSyncMigrationValue(t, connection, "SELECT COUNT(*) FROM file_versions") != "3" {
		t.Fatal("验证后成功写入的历史记录不符", value)
	}
	t.Log("零字节、微秒时间和同对象多引用通过；重复、跨账号、非法时间和删除被引用记录均受约束")
}
