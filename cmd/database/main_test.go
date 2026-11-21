package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/migrations"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func verificationDB(t *testing.T) *sql.DB {
	t.Helper()
	name := os.Getenv("CLOUDNEST_SCHEMA_TEST_DB")
	if name == "" {
		t.Skip("set CLOUDNEST_SCHEMA_TEST_DB to an isolated verification database")
	}
	if !strings.HasPrefix(name, "cloudnest_migrate_verify_") {
		t.Fatal("verification requires cloudnest_migrate_verify_ database prefix")
	}
	config := mysql.NewConfig()
	config.User = envOrDefault("CLOUDNEST_DB_USER", "root")
	config.Passwd = envOrDefault("CLOUDNEST_DB_PASSWORD", "root")
	config.Net = "tcp"
	config.Addr = envOrDefault("CLOUDNEST_DB_ADDRESS", "127.0.0.1:13309")
	config.DBName = name
	config.ParseTime = true
	config.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestSchemaConstraints(t *testing.T) {
	database := verificationDB(t)
	transaction, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	insert := func(query string, args ...any) int64 {
		t.Helper()
		result, err := transaction.Exec(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	reject := func(label string, code uint16, query string, args ...any) {
		t.Helper()
		_, err := transaction.Exec(query, args...)
		var mysqlError *mysql.MySQLError
		if !errors.As(err, &mysqlError) || mysqlError.Number != code {
			t.Fatalf("%s: expected MySQL %d, got %v", label, code, err)
		}
		t.Log("rejected", label)
	}
	userA := insert("INSERT INTO users(username,display_name,password_hash) VALUES('verify_a','A','test-hash')")
	userB := insert("INSERT INTO users(username,display_name,password_hash) VALUES('verify_b','B','test-hash')")
	rootA := insert("INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userA)
	rootB := insert("INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userB)
	reject("duplicate root", 1062, "INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userA)
	reject("cross-user parent", 1452, "INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(?,?,'bad','bad','DIR')", userA, rootB)
	reject("invalid root status", 3819, "UPDATE file_entries SET status='TRASHED',deleted_at=NOW(6) WHERE id=?", rootA)
	reject("quota overflow", 3819, "UPDATE users SET reserved_storage=storage_quota+1 WHERE id=?", userA)
	reject("invalid username", 3819, "INSERT INTO users(username,display_name,password_hash) VALUES('Upper','A','test')")
	key := []byte(norm.NFC.String(cases.Fold().String(norm.NFC.String("Cafe\u0301"))))
	folder := insert("INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(?,?,'Café',?,'DIR')", userA, rootA, key)
	reject("normalized duplicate name", 1062, "INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(?,?,'CAFÉ',?,'DIR')", userA, rootA, key)
	insert("UPDATE file_entries SET status='TRASHED',deleted_at=NOW(6) WHERE id=?", folder)
	replacement := insert("INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(?,?,'CAFÉ',?,'DIR')", userA, rootA, key)
	insert("UPDATE file_entries SET status='TRASHED',deleted_at=NOW(6) WHERE id=?", replacement)
	deviceA := insert("INSERT INTO user_devices(user_id,installation_id,name,platform) VALUES(?,'00000000-0000-0000-0000-000000000001','A','WEB')", userA)
	deviceB := insert("INSERT INTO user_devices(user_id,installation_id,name,platform) VALUES(?,'00000000-0000-0000-0000-000000000002','B','WEB')", userB)
	reject("cross-user session device", 1452, "INSERT INTO auth_sessions(user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,UNHEX(SHA2('token',256)),NOW()+INTERVAL 1 DAY)", userA, deviceB)
	objectA := insert("INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES(?,'local_minio','cloudnest','verify/a',7,'READY',UNHEX(SHA2('a',256)),NOW(6))", userA)
	objectB := insert("INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,file_size) VALUES(?,'local_minio','cloudnest','verify/b',7)", userB)
	reject("unverified ready object", 3819, "UPDATE stored_objects SET status='READY' WHERE id=?", objectB)
	reject("duplicate ready content", 1062, "INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES(?,'local_minio','cloudnest','verify/duplicate',7,'READY',UNHEX(SHA2('a',256)),NOW(6))", userA)
	reject("cross-user object", 1452, "INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,?,'bad.txt','bad.txt','FILE','DOCUMENT',?,7)", userB, rootB, objectA)
	fileA := insert("INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,?,'a.txt','a.txt','FILE','DOCUMENT',?,7)", userA, rootA, objectA)
	reject("cross-user preference", 1452, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(?,?,NOW(6))", userB, fileA)
	reject("folder carrying content", 3819, "UPDATE file_entries SET object_id=? WHERE id=?", objectA, folder)
	reject("delete referenced object", 1451, "DELETE FROM stored_objects WHERE id=?", objectA)
	insert("INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(?,?,NOW(6),NOW(6))", userA, fileA)
	var revision uint64
	if err := transaction.QueryRow("SELECT revision FROM file_entries WHERE id=?", fileA).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("preference changed file revision", err)
	}
	task := insert("INSERT INTO upload_tasks(user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,reserved_bytes,expires_at,status) VALUES(?,?,?,?,'a.txt','SINGLE',7,7,NOW()+INTERVAL 1 DAY,'VERIFYING')", userA, deviceA, objectA, rootA)
	reject("terminal reserved bytes", 3819, "UPDATE upload_tasks SET status='FAILED' WHERE id=?", task)
	reject("incomplete completion", 3819, "UPDATE upload_tasks SET status='COMPLETED',reserved_bytes=0 WHERE id=?", task)
	reject("invalid multipart size", 3819, "UPDATE upload_tasks SET upload_mode='MULTIPART',part_size=10,part_count=1 WHERE id=?", task)
	insert("UPDATE users SET reserved_storage=7 WHERE id=?", userA)
	for attempt := 0; attempt < 2; attempt++ {
		result, err := transaction.Exec("UPDATE upload_tasks SET status='COMPLETED',reserved_bytes=0,published_object_id=?,result_entry_id=?,completed_at=NOW(6) WHERE id=? AND status='VERIFYING'", objectA, fileA, task)
		if err != nil {
			t.Fatal(err)
		}
		affected, _ := result.RowsAffected()
		if affected == 1 {
			insert("UPDATE users SET reserved_storage=reserved_storage-7,used_storage=used_storage+7 WHERE id=?", userA)
		}
	}
	var used, reserved uint64
	if err := transaction.QueryRow("SELECT used_storage,reserved_storage FROM users WHERE id=?", userA).Scan(&used, &reserved); err != nil || used != 7 || reserved != 0 {
		t.Fatal("completion was not idempotent", used, reserved, err)
	}
	insert("UPDATE file_entries SET status='TRASHED',deleted_at=NOW(6) WHERE id=?", fileA)
	var bytes uint64
	if err := transaction.QueryRow("SELECT SUM(file_size) FROM file_entries WHERE user_id=? AND entry_type='FILE' AND status<>'PURGED'", userA).Scan(&bytes); err != nil || bytes != 7 {
		t.Fatal("trash accounting", err)
	}
	insert("SAVEPOINT before_purge")
	insert("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=NOW(6) WHERE id=?", fileA)
	insert("UPDATE users SET used_storage=used_storage-7 WHERE id=?", userA)
	insert("ROLLBACK TO SAVEPOINT before_purge")
	var status string
	if err := transaction.QueryRow("SELECT status FROM file_entries WHERE id=?", fileA).Scan(&status); err != nil || status != "TRASHED" {
		t.Fatal("rollback failed", err)
	}
	insert("INSERT INTO idempotency_records(user_id,operation,idempotency_key,request_hash,resource_type,response_status,expires_at) VALUES(?,'create','test-key',UNHEX(SHA2('request',256)),'ENTRY',201,NOW()+INTERVAL 7 DAY)", userA)
	reject("duplicate idempotency key", 1062, "INSERT INTO idempotency_records(user_id,operation,idempotency_key,request_hash,resource_type,response_status,expires_at) VALUES(?,'create','test-key',UNHEX(SHA2('request',256)),'ENTRY',201,NOW()+INTERVAL 7 DAY)", userA)
	t.Log("PASS same-name trash, preference isolation, idempotent quota settlement, rollback")
}

func TestConcurrentQuotaReservation(t *testing.T) {
	database := verificationDB(t)
	result, err := database.Exec("INSERT INTO users(username,display_name,password_hash,storage_quota) VALUES('quota_race','Race','test',100)")
	if err != nil {
		t.Fatal(err)
	}
	userID, _ := result.LastInsertId()
	defer database.Exec("DELETE FROM users WHERE id=?", userID)
	var accepted atomic.Int32
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	for worker := 0; worker < 2; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			transaction, err := database.BeginTx(ctx, nil)
			if err != nil {
				failures <- err
				return
			}
			defer transaction.Rollback()
			var used, reserved, quota uint64
			err = transaction.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,storage_quota FROM users WHERE id=? FOR UPDATE", userID).Scan(&used, &reserved, &quota)
			if err != nil {
				failures <- err
				return
			}
			if used+reserved+70 > quota {
				return
			}
			if _, err = transaction.ExecContext(ctx, "UPDATE users SET reserved_storage=reserved_storage+70 WHERE id=?", userID); err != nil {
				failures <- err
				return
			}
			if err = transaction.Commit(); err != nil {
				failures <- err
				return
			}
			accepted.Add(1)
		}()
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if accepted.Load() != 1 {
		t.Fatalf("expected one accepted reservation, got %d", accepted.Load())
	}
}

func TestMigrationAndBootstrapGuards(t *testing.T) {
	database := verificationDB(t)
	ctx := context.Background()
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var originalHash string
	if err := connection.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE username='root'").Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(originalHash), []byte("root")); err != nil {
		t.Fatal("bootstrap password does not verify", err)
	}
	if err := seedAccount(ctx, connection, "different-password"); err != nil {
		t.Fatal(err)
	}
	var currentHash string
	connection.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE username='root'").Scan(&currentHash)
	if currentHash != originalHash {
		t.Fatal("repeated seed changed the existing password")
	}
	initialMigration := migrations.Ordered()[0]
	if err := migrate(ctx, connection, initialMigration); err != nil {
		t.Fatal(err)
	}
	var checksum string
	connection.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version=1").Scan(&checksum)
	defer connection.ExecContext(ctx, "UPDATE schema_migrations SET dirty=false,checksum=? WHERE version=1", checksum)
	connection.ExecContext(ctx, "UPDATE schema_migrations SET dirty=true WHERE version=1")
	if err := migrate(ctx, connection, initialMigration); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatal("dirty migration not rejected", err)
	}
	connection.ExecContext(ctx, "UPDATE schema_migrations SET dirty=false,checksum=REPEAT('0',64) WHERE version=1")
	if err := migrate(ctx, connection, initialMigration); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal("changed migration not rejected", err)
	}
}
