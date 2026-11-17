package repository

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

// uploadTaskDatabase 安装正式任务表和关联夹具，供读取及服务联测共用。
func uploadTaskDatabase(t *testing.T) (*sql.DB, domain.UploadTask) {
	t.Helper()
	// 1. 在独占库安装正式任务表，建立同用户设备、对象及目录引用。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE upload_tasks (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if start < 0 || end <= start {
		t.Fatal("正式上传任务表定义未找到")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(7,'upload_owner','上传用户','fixture_hash','ACTIVE'),(8,'upload_other','其他用户','fixture_hash','DISABLED')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB'),(12,8,'38ee758a-0727-42de-ae7d-3bb33155e148','other','ANDROID')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,'MINIO','fixture-bucket','upload-staged',16777216),(24,8,'MINIO','fixture-bucket','other-staged',0)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at) VALUES(29,7,'MINIO','fixture-bucket','upload-published',16777216,UNHEX(REPEAT('00',32)),'READY','2026-11-17 08:00:00.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(13,7,NULL,'',X'','DIR'),(14,8,NULL,'',X'','DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(31,7,13,'发布笔记.MD',CAST('发布笔记.MD' AS BINARY),'FILE','OTHER',29,16777216)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	want, values := uploadTaskReadFixture()
	arguments := make([]any, len(values))
	for index, value := range values {
		arguments[index] = value
	}
	statement := "INSERT INTO upload_tasks(" + strings.Join((&uploadTaskMappingRows{}).Columns(), ",") + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
	if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
		t.Fatal(err)
	}
	return pool, want
}

func TestFindUserUploadTaskIntegration(t *testing.T) {
	pool, want := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, expected domain.UploadTask) {
		t.Helper()
		actual, err := store.FindUserUploadTask(ctx, expected.UserID, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatal("正式任务二十八字段读回错误", err)
		}
	}
	check(t, want)

	// 2. 十种阶段、到期记录及已清理结果继续保留数据库事实。
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=? WHERE user_id=7 AND id=19", string(status)); err != nil {
				t.Fatal(err)
			}
			want.Status = status
			check(t, want)
		})
	}
	t.Run("expired retained and copies", func(t *testing.T) {
		want.ExpiresAt = want.CreatedAt.Add(-24 * time.Hour)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET expires_at=? WHERE user_id=7 AND id=19", want.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		check(t, want)
		first, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		*first.PublishedObjectID, first.ExpectedSHA256[0], *first.LeaseOwner = 999, 0xff, "changed"
		check(t, want)
	})
	t.Run("completed result tombstone", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=? WHERE user_id=7 AND id=31", want.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='COMPLETED' WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		want.Status = domain.UploadStatusCompleted
		check(t, want)
	})

	t.Run("consistent read under uncommitted write", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE upload_tasks SET last_error_message='uncommitted' WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		actual, err := store.FindUserUploadTask(readContext, 7, 19)
		if err != nil || !reflect.DeepEqual(actual, &want) {
			t.Fatal("任务读取未保留已提交快照或被外部写锁阻塞", err)
		}
	})

	// 3. 缺失及跨账号目标共享缺失错误，零字节 SINGLE 保留全部空值。
	if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at,created_at,updated_at) VALUES(20,8,12,24,14,'empty.bin','SINGLE',0,'UPLOADING',0,?,?,?)", want.ExpiresAt, want.CreatedAt, want.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][2]uint64{{7, 20}, {8, 19}, {7, 999}} {
		actual, err := store.FindUserUploadTask(ctx, scope[0], scope[1])
		if actual != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("缺失或跨账号上传任务未统一隐藏", err)
		}
	}
	check(t, domain.UploadTask{ID: 20, UserID: 8, DeviceID: 12, StagedObjectID: 24, TargetParentID: 14,
		RequestedName: "empty.bin", UploadMode: "SINGLE", Status: domain.UploadStatusUploading,
		ExpiresAt: want.ExpiresAt, CreatedAt: want.CreatedAt, UpdatedAt: want.UpdatedAt})

	// 4. 最大无符号标识在普通夹具之后写入，并核对真实 MySQL 参数与扫描。
	t.Run("full unsigned range", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'upload_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','maximum','WEB')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','maximum',?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_version,attempt_count,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',?,'UPLOADING',?,?,4294967295,?,?,?)", maximum, maximum, maximum, maximum, maximum, maximum, maximum, maximum, want.ExpiresAt, want.CreatedAt, want.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		check(t, domain.UploadTask{ID: maximum, UserID: maximum, DeviceID: maximum, StagedObjectID: maximum, TargetParentID: maximum,
			RequestedName: "maximum.bin", UploadMode: "SINGLE", FileSize: maximum, Status: domain.UploadStatusUploading,
			ReservedBytes: maximum, LeaseVersion: maximum, AttemptCount: ^uint32(0),
			ExpiresAt: want.ExpiresAt, CreatedAt: want.CreatedAt, UpdatedAt: want.UpdatedAt})
	})
	t.Run("closed pool is database failure", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		actual, err := store.FindUserUploadTask(ctx, 7, 19)
		if actual != nil || err == nil || errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("数据库故障被混同为任务缺失", err)
		}
	})
}
