package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserStoredObjectIntegration(t *testing.T) {
	// 1. 沿用独占库、正式对象表及自动清理复核入口。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	want, _ := storedObjectReadFixture()
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,status) VALUES(7,'object_owner','对象用户','fixture_hash','ACTIVE'),(8,'object_foreign','另一用户','fixture_hash','DISABLED')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,file_size,verified_sha256,status,storage_etag,verified_at,unreferenced_at,gc_after,last_error_code,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		want.ID, want.OwnerUserID, want.Provider, want.Bucket, want.ObjectKey, want.StorageVersionID,
		want.FileSize, want.VerifiedSHA256[:], string(want.Status), want.StorageETag,
		want.VerifiedAt, want.UnreferencedAt, want.GCAfter, want.LastErrorCode, want.CreatedAt, want.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, expected domain.StoredObject) {
		t.Helper()
		actual, err := store.FindUserStoredObject(ctx, expected.OwnerUserID, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatal("真实 MySQL 存储对象未完整读回", err)
		}
	}

	// 2. 六种状态核对数据库自动生成的去重摘要及保留的验证事实。
	for _, status := range []domain.StoredObjectStatus{
		domain.StoredObjectStatusStaging, domain.StoredObjectStatusVerifying, domain.StoredObjectStatusReady,
		domain.StoredObjectStatusOrphaned, domain.StoredObjectStatusDeleting, domain.StoredObjectStatusDeleted,
	} {
		t.Run(string(status), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET status=? WHERE owner_user_id=7 AND id=23", string(status)); err != nil {
				t.Fatal(err)
			}
			want.Status, want.DedupHash = status, nil
			if status == domain.StoredObjectStatusReady {
				want.DedupHash = want.VerifiedSHA256
			}
			check(t, want)
		})
	}
	t.Run("independent copies and read-only record", func(t *testing.T) {
		first, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		*first.StorageVersionID, first.VerifiedSHA256[0], *first.GCAfter = "changed", 0xff, time.Time{}
		check(t, want)
	})
	t.Run("consistent read under uncommitted write", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE stored_objects SET last_error_code='UNCOMMITTED' WHERE owner_user_id=7 AND id=23"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		actual, err := store.FindUserStoredObject(readContext, 7, 23)
		if err != nil || !reflect.DeepEqual(actual, &want) {
			t.Fatal("对象读取未保留已提交快照或被外部写锁阻塞", err)
		}
	})
	t.Run("nullable zero-byte object and ownership", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,created_at,updated_at) VALUES(24,8,'MINIO','fixture-bucket','objects/empty',0,?,?)", want.CreatedAt, want.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		check(t, domain.StoredObject{ID: 24, OwnerUserID: 8, Provider: "MINIO", Bucket: "fixture-bucket",
			ObjectKey: "objects/empty", Status: domain.StoredObjectStatusStaging, CreatedAt: want.CreatedAt, UpdatedAt: want.UpdatedAt})
		for _, scope := range [][2]uint64{{7, 24}, {8, 23}, {7, 999}} {
			actual, err := store.FindUserStoredObject(ctx, scope[0], scope[1])
			if actual != nil || !errors.Is(err, domain.ErrStoredObjectNotFound) {
				t.Fatal("跨用户或缺失对象未返回统一缺失错误", err)
			}
		}
	})
	t.Run("cancelled query", func(t *testing.T) {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		actual, err := store.FindUserStoredObject(cancelled, 7, 23)
		if actual != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("真实对象读取未保留取消原因", err)
		}
	})

	// 3. 完整无符号边界在普通夹具后插入，随后核对关闭连接池故障。
	t.Run("full unsigned range and database dates", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'object_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		createdAt := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		updatedAt := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,created_at,updated_at) VALUES(?,?,'MINIO','fixture-bucket','objects/maximum',?,?,?)", maximum, maximum, maximum, createdAt, updatedAt); err != nil {
			t.Fatal(err)
		}
		check(t, domain.StoredObject{ID: maximum, OwnerUserID: maximum, Provider: "MINIO", Bucket: "fixture-bucket",
			ObjectKey: "objects/maximum", FileSize: maximum, Status: domain.StoredObjectStatusStaging, CreatedAt: createdAt, UpdatedAt: updatedAt})
	})
	t.Run("closed pool is database failure", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		actual, err := store.FindUserStoredObject(ctx, 7, 23)
		if actual != nil || err == nil || errors.Is(err, domain.ErrStoredObjectNotFound) {
			t.Fatal("数据库故障被当作对象不存在", err)
		}
	})
}
