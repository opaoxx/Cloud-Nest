package main

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestUploadPartsMigrationUpgrade(t *testing.T) {
	connection := openEmptyMigrationFixture(t)
	ctx := t.Context()
	execSQL := func(statement string) {
		t.Helper()
		if _, err := connection.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	read := func(query string) string {
		t.Helper()
		var value string
		if err := connection.QueryRowContext(ctx, query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	// 1. 用正式版本 1 SQL 初始化旧库，留下已发布文件与仍有预占的 SINGLE 任务。
	catalog := migrations.Ordered()
	if err := migrate(ctx, connection, catalog[:1]); err != nil {
		t.Fatal(err)
	}
	execSQL(`INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage)
		VALUES(7,'fixture_user','Fixture','fixture-hash',7,3)`)
	execSQL(`INSERT INTO user_devices(id,user_id,installation_id,name,platform)
		VALUES(11,7,'00000000-0000-4000-8000-000000000001','Fixture','WEB')`)
	execSQL(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at)
		VALUES(21,7,'local_minio','fixture','b3-published',7,'READY',UNHEX(SHA2('fixture',256)),UTC_TIMESTAMP(6)),
		      (22,7,'local_minio','fixture','b3-staged',3,'STAGING',NULL,NULL)`)
	execSQL("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(31,7,NULL,'',X'','DIR')")
	execSQL(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES(32,7,31,'b3.txt','b3.txt','FILE','DOCUMENT',21,7)`)
	execSQL(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,
		upload_mode,file_size,status,reserved_bytes,expires_at)
		VALUES(41,7,11,22,31,'pending.bin','SINGLE',3,'UPLOADING',3,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
	const businessSnapshot = `SELECT JSON_ARRAY(u.id,u.used_storage,u.reserved_storage,e.id,e.name,e.parent_id,o.id,o.object_key,
		o.file_size,t.id,t.upload_mode,t.status,t.staged_object_id,t.reserved_bytes,t.expires_at)
		FROM users u JOIN file_entries e ON e.user_id=u.id JOIN stored_objects o ON o.id=e.object_id
		JOIN upload_tasks t ON t.user_id=u.id WHERE u.id=7 AND e.id=32 AND t.id=41`
	const initialRecord = `SELECT JSON_ARRAY(version,name,checksum,dirty,started_at,applied_at)
		FROM schema_migrations WHERE version=1`
	beforeUpgrade, beforeRecord := read(businessSnapshot), read(initialRecord)

	// 2. 经正式命令入口升级并重跑，确认版本 1 的数据和已应用记录未重写。
	databaseName := read("SELECT DATABASE()")
	for attempt := 0; attempt < 2; attempt++ {
		if err := run(databaseName, false); err != nil {
			t.Fatal(err)
		}
		if read(businessSnapshot) != beforeUpgrade || read(initialRecord) != beforeRecord {
			t.Fatal("升级或重跑改变了版本 1 的数据或迁移记录")
		}
		assertFixtureMigrationRecords(t, connection, catalog, len(catalog))
	}
	if value := read("SELECT COUNT(*) FROM upload_parts"); value != "0" {
		t.Fatalf("升级后的回执表预期为空，实际 %s 行", value)
	}

	// 3. 在合成 Multipart 任务下验证最小与最大编号、可空摘要及数据库拒绝路径。
	execSQL(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
		VALUES(23,7,'local_minio','fixture','b4-staged',52428800000)`)
	execSQL(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,
		upload_mode,file_size,part_size,part_count,reserved_bytes,expires_at)
		VALUES(42,7,11,23,31,'parts.bin','MULTIPART',52428800000,5242880,10000,0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
	const insertPart = "INSERT INTO upload_parts(task_id,part_number,etag,size,checksum_algorithm,checksum_value,observed_at) VALUES(?,?,'CaseSensitive',5242880,?,?,'2026-11-21 00:00:00.123456')"
	for _, part := range []struct {
		number         int
		algorithm, sum any
	}{{1, "SHA256", "YWJjZA=="}, {10000, nil, nil}} {
		if _, err := connection.ExecContext(ctx, insertPart, 42, part.number, part.algorithm, part.sum); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct {
		name           string
		task, number   int
		algorithm, sum any
		code           uint16
	}{
		{name: "duplicate", task: 42, number: 1, code: 1062},
		{name: "unknown task", task: 999, number: 2, code: 1452},
		{name: "part zero", task: 42, number: 0, code: 3819},
		{name: "part 10001", task: 42, number: 10001, code: 3819},
		{name: "algorithm only", task: 42, number: 2, algorithm: "SHA256", code: 3819},
		{name: "checksum only", task: 42, number: 2, sum: "YWJjZA==", code: 3819},
	} {
		_, err := connection.ExecContext(ctx, insertPart, testCase.task, testCase.number, testCase.algorithm, testCase.sum)
		var mysqlError *mysql.MySQLError
		if !errors.As(err, &mysqlError) || mysqlError.Number != testCase.code {
			t.Fatalf("%s: expected MySQL %d, got %v", testCase.name, testCase.code, err)
		}
		t.Logf("%s rejected with MySQL %d", testCase.name, mysqlError.Number)
	}
	receipt := read("SELECT CONCAT_WS('|',etag,size,checksum_algorithm,checksum_value,observed_at) FROM upload_parts WHERE task_id=42 AND part_number=1")
	if receipt != "CaseSensitive|5242880|SHA256|YWJjZA==|2026-11-21 00:00:00.123456" ||
		read("SELECT COUNT(*) FROM upload_parts") != "2" ||
		read("SELECT COUNT(*) FROM upload_parts WHERE part_number=10000 AND checksum_algorithm IS NULL AND checksum_value IS NULL") != "1" {
		t.Fatalf("unexpected receipt or row count: %s", receipt)
	}
	if read(businessSnapshot) != beforeUpgrade {
		t.Fatal("回执夹具改变了 B3 数据快照")
	}
}
