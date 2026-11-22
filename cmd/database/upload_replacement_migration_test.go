package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestUploadReplacementMigrationIntegration(t *testing.T) {
	catalog := migrations.Ordered()
	t.Run("empty schema and rerun", func(t *testing.T) {
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		verifySyncMigrationAndRerun(t, connection, databaseName, catalog)
		columns := readSyncMigrationValue(t, connection, `SELECT GROUP_CONCAT(CONCAT_WS(':',COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,
			IFNULL(COLUMN_DEFAULT,'NULL')) ORDER BY ORDINAL_POSITION)
			FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='upload_tasks'
			AND COLUMN_NAME IN ('replacement_entry_id','replacement_expected_revision','replacement_published_revision','replacement_conflicting_revision')`)
		if columns != "replacement_entry_id:bigint unsigned:YES:NULL,replacement_expected_revision:bigint unsigned:YES:NULL,"+
			"replacement_published_revision:bigint unsigned:YES:NULL,replacement_conflicting_revision:bigint unsigned:YES:NULL" {
			t.Fatal("替换任务四列的类型或默认值不符", columns)
		}
		t.Log("空库已执行完整清单，四列为可空 unsigned bigint，重跑保持原账本")
	})

	t.Run("upgrade v4 preserves ordinary uploads", func(t *testing.T) {
		// 1. 在合法 v4 中准备待完成分片任务、已完成普通任务和历史行。
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		if err := migrate(t.Context(), connection, catalog[:4]); err != nil {
			t.Fatal(err)
		}
		seedSyncMigrationBusinessData(t, connection)
		executeSyncMigrationSQL(t, connection, `INSERT INTO upload_tasks
			(id,user_id,device_id,staged_object_id,published_object_id,target_parent_id,requested_name,resolved_name,
			 upload_mode,file_size,status,result_entry_id,reserved_bytes,expires_at,created_at,updated_at,completed_at)
			VALUES(42,7,11,21,21,31,'b4.txt','b4.txt','SINGLE',7,'COMPLETED',32,0,
			 '2026-11-23 00:00:00','2026-11-22 00:00:00','2026-11-22 00:01:00','2026-11-22 00:01:00')`)
		executeSyncMigrationSQL(t, connection, `INSERT INTO file_versions(user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
			VALUES(7,32,1,21,7,'2026-11-22 00:00:00.123456','2026-12-22 00:00:00.123456')`)
		const taskSnapshot = `SELECT GROUP_CONCAT(JSON_ARRAY(id,user_id,device_id,staged_object_id,published_object_id,target_parent_id,
			requested_name,resolved_name,upload_mode,storage_upload_id,HEX(expected_sha256),file_size,part_size,part_count,status,result_entry_id,
			reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,attempt_count,expires_at,last_error_code,last_error_message,
			created_at,updated_at,completed_at) ORDER BY id SEPARATOR '\n') FROM upload_tasks`
		const historySnapshot = `SELECT JSON_ARRAY(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until) FROM file_versions`
		tasksBefore := readSyncMigrationValue(t, connection, taskSnapshot)
		businessBefore := readSyncMigrationValue(t, connection, syncMigrationBusinessSnapshot)
		historyBefore := readSyncMigrationValue(t, connection, historySnapshot)
		ledgerBefore := readSyncMigrationValue(t, connection, syncMigrationLedger)
		t.Setenv("CLOUDNEST_SCHEMA_TEST_DB", databaseName)
		if err := repository.CheckSchema(t.Context(), verificationDB(t)); err == nil || !strings.Contains(err.Error(), "迁移版本 5 的记录缺失") {
			t.Fatal("服务启动校验未拒绝仅到 v4 的库", err)
		}

		// 2. 通过正式入口补 v5 并复验四列全空，旧字段、B4 分片回执和历史引用不变。
		verifySyncMigrationAndRerun(t, connection, databaseName, catalog)
		if readSyncMigrationValue(t, connection, taskSnapshot) != tasksBefore ||
			readSyncMigrationValue(t, connection, syncMigrationBusinessSnapshot) != businessBefore ||
			readSyncMigrationValue(t, connection, historySnapshot) != historyBefore ||
			readSyncMigrationValue(t, connection, syncMigrationLedger+" WHERE version<=4") != ledgerBefore ||
			readSyncMigrationValue(t, connection, `SELECT COUNT(*) FROM upload_tasks
				WHERE replacement_entry_id IS NULL AND replacement_expected_revision IS NULL
					AND replacement_published_revision IS NULL AND replacement_conflicting_revision IS NULL`) != "2" {
			t.Fatal("v5 或重跑改写了既有普通任务、配额、历史或账本")
		}
		t.Log("合法 v4 只补 v5，原 28 列数据、任务预占、分片回执、历史和前四版记录均一致")

		// 3. 在同一个隔离升级库中直接验收新约束。
		verifyUploadReplacementConstraints(t, connection)
	})

	t.Run("DDL failure leaves version 5 dirty", func(t *testing.T) {
		connection := openEmptyMigrationFixture(t)
		databaseName := readSyncMigrationValue(t, connection, "SELECT DATABASE()")
		if err := migrate(t.Context(), connection, catalog[:4]); err != nil {
			t.Fatal(err)
		}
		executeSyncMigrationSQL(t, connection, "ALTER TABLE upload_tasks ADD COLUMN replacement_conflicting_revision INT NULL")
		migrationError := run(databaseName, false)
		requireSyncMigrationMySQLError(t, migrationError, 1060)
		if !strings.Contains(migrationError.Error(), "migration 5 failed and is marked dirty") {
			t.Fatal(migrationError)
		}
		assertFixtureMigrationRecords(t, connection, catalog, 4)
		dirtyLedger := readSyncMigrationValue(t, connection, syncMigrationLedger)
		if err := run(databaseName, false); err == nil || !strings.Contains(err.Error(), "migration 5 is dirty") {
			t.Fatal("迁移重跑未拒绝 dirty 的版本 5", err)
		}
		t.Setenv("CLOUDNEST_SCHEMA_TEST_DB", databaseName)
		if err := repository.CheckSchema(t.Context(), verificationDB(t)); err == nil ||
			!strings.Contains(err.Error(), "迁移版本 5 的 dirty=true") {
			t.Fatal("启动校验未拒绝 dirty 的版本 5", err)
		}
		if readSyncMigrationValue(t, connection, syncMigrationLedger) != dirtyLedger ||
			readSyncMigrationValue(t, connection, `SELECT GROUP_CONCAT(CONCAT(COLUMN_NAME,':',COLUMN_TYPE)) FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='upload_tasks' AND COLUMN_NAME LIKE 'replacement%'`) != "replacement_conflicting_revision:int" {
			t.Fatal("失败重跑改变了迁移账本或 DDL 现场")
		}
		t.Log("重复列返回 1060，单条 ALTER 无部分新增列，v5 保留 dirty")
	})
}

func verifyUploadReplacementConstraints(t *testing.T, connection *sql.Conn) {
	t.Helper()
	// 1. 先核对绑定成对、摘要必填及同账号归属。
	executeSyncMigrationSQL(t, connection, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (34,8,NULL,'',X'','DIR')")
	const updateTask = "UPDATE upload_tasks SET "
	for _, sample := range []struct {
		assignments string
		code        uint16
	}{
		{"replacement_entry_id=32", 3819},
		{"replacement_expected_revision=4", 3819},
		{"replacement_entry_id=32,replacement_expected_revision=0", 3819},
		{"replacement_entry_id=32,replacement_expected_revision=4,expected_sha256=NULL", 3819},
		{"replacement_entry_id=34,replacement_expected_revision=4", 1452},
		{"replacement_entry_id=999,replacement_expected_revision=4", 1452},
		{"replacement_conflicting_revision=5", 3819},
	} {
		_, err := connection.ExecContext(t.Context(), updateTask+sample.assignments+" WHERE id=41")
		requireSyncMigrationMySQLError(t, err, sample.code)
	}
	executeSyncMigrationSQL(t, connection, updateTask+"replacement_entry_id=32,replacement_expected_revision=4 WHERE id=41")

	// 2. 用同一任务隔离验证两类结果，回滚成功样本再试冲突分支。
	const completed = "status='COMPLETED',reserved_bytes=0,published_object_id=21,completed_at=UTC_TIMESTAMP(6),"
	const conflicted = "status='FAILED',reserved_bytes=0,last_error_code='REVISION_CONFLICT',"
	for _, assignments := range []string{
		"replacement_published_revision=5",
		completed + "result_entry_id=31,replacement_published_revision=5",
		completed + "result_entry_id=32,replacement_published_revision=NULL",
		completed + "result_entry_id=32,replacement_published_revision=6",
		completed + "result_entry_id=32,replacement_published_revision=5,replacement_conflicting_revision=6",
		completed + "result_entry_id=32,replacement_published_revision=1,replacement_expected_revision=18446744073709551615",
		conflicted + "replacement_conflicting_revision=NULL",
		conflicted + "replacement_conflicting_revision=4",
		"status='FAILED',reserved_bytes=0,last_error_code=NULL,replacement_conflicting_revision=5",
	} {
		_, err := connection.ExecContext(t.Context(), updateTask+assignments+" WHERE id=41")
		requireSyncMigrationMySQLError(t, err, 3819)
	}
	executeSyncMigrationSQL(t, connection, "START TRANSACTION")
	executeSyncMigrationSQL(t, connection, updateTask+completed+"result_entry_id=32,replacement_published_revision=5 WHERE id=41")
	if value := readSyncMigrationValue(t, connection, `SELECT CONCAT_WS('|',status,result_entry_id,replacement_published_revision)
		FROM upload_tasks WHERE id=41`); value != "COMPLETED|32|5" {
		t.Fatal("替换成功版本快照不符", value)
	}
	executeSyncMigrationSQL(t, connection, "ROLLBACK")
	executeSyncMigrationSQL(t, connection, updateTask+conflicted+"replacement_conflicting_revision=5 WHERE id=41")
	if value := readSyncMigrationValue(t, connection, `SELECT CONCAT_WS('|',status,last_error_code,replacement_conflicting_revision)
		FROM upload_tasks WHERE id=41`); value != "FAILED|REVISION_CONFLICT|5" {
		t.Fatal("替换冲突版本快照不符", value)
	}
	t.Log("同账号绑定、发布版本与冲突快照通过，跨账号、缺项、错误阶段及 uint64 上限递增被拒")
}
