package repository

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindReusableUploadObjectResults(t *testing.T) {
	queryFailure := errors.New("fixture query failure")
	for _, scenario := range []struct {
		name      string
		missing   bool
		changes   map[int]driver.Value
		wantError error
	}{
		{name: "match"},
		{name: "missing", missing: true},
		{name: "query failure", wantError: queryFailure},
		{name: "wrong owner", changes: map[int]driver.Value{1: int64(8)}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong size", changes: map[int]driver.Value{6: int64(2049)}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong digest", changes: map[int]driver.Value{7: make([]byte, 32), 9: make([]byte, 32)}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong state", changes: map[int]driver.Value{8: "ORPHANED", 9: nil}, wantError: domain.ErrInvalidStoredObjectData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			want, values := storedObjectReadFixture()
			digest := domain.UploadContentDigest{SizeBytes: want.FileSize, SHA256: *want.VerifiedSHA256}
			for column, value := range scenario.changes {
				values[column] = value
			}
			if scenario.missing {
				values = nil
			}
			script := &loginTransactionScript{status: "ACTIVE"}
			script.objectQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				expectedScope := "WHERE owner_user_id = ? AND dedup_hash = ? AND file_size = ? AND status = ? LIMIT ? FOR UPDATE"
				if !strings.HasSuffix(statement, expectedScope) || len(arguments) != 5 {
					t.Fatal("可复用对象查询范围或行锁错误", statement, arguments)
				}
				boundHash, binaryHash := arguments[1].Value.([]byte)
				if arguments[0].Value != want.OwnerUserID || !binaryHash || !bytes.Equal(boundHash, digest.SHA256[:]) ||
					arguments[2].Value != digest.SizeBytes || arguments[3].Value != "READY" || arguments[4].Value != int64(1) {
					t.Fatal("可复用对象参数错误", arguments)
				}
				if scenario.wantError == queryFailure {
					return nil, queryFailure
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			store := loginTransactionTestStore(t, script)
			ctx := context.Background()
			if object, err := store.FindReusableUploadObject(ctx, digest); object != nil || err == nil {
				t.Fatal("事务外查询应被拒绝", err)
			}
			err := store.withActiveUserTransaction(ctx, want.OwnerUserID, func(txContext context.Context, txStore *Store) error {
				cancelled, cancel := context.WithCancel(txContext)
				cancel()
				if object, err := txStore.FindReusableUploadObject(cancelled, digest); object != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("查询未保留取消原因", err)
				}
				object, err := txStore.FindReusableUploadObject(txContext, digest)
				if values == nil || scenario.wantError != nil {
					if object != nil {
						t.Fatal("未命中或故障时返回了对象", object)
					}
				} else if !reflect.DeepEqual(object, &want) {
					t.Fatal("命中对象未完整读回", err)
				}
				return err
			})
			expectedSequence := "begin,lock,object,commit"
			if scenario.wantError != nil {
				expectedSequence = "begin,lock,object,rollback"
			}
			if !errors.Is(err, scenario.wantError) || script.sequence() != expectedSequence {
				t.Fatal("查询结果或事务顺序错误", err, script.sequence())
			}
		})
	}
}

func TestFindReusableUploadObjectIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	// 同摘要的不同大小由不同唯一键承载，回收计时中的 READY 仍可命中。
	if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects
		(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at,unreferenced_at,gc_after)
		VALUES (30,7,'MINIO','fixture-bucket','dedup-empty',0,?, 'READY',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		       (31,7,'MINIO','fixture-bucket','dedup-orphan',1,?, 'ORPHANED',UTC_TIMESTAMP(6),NULL,NULL),
		       (32,8,'MINIO','fixture-bucket','dedup-foreign',2,?, 'READY',UTC_TIMESTAMP(6),NULL,NULL)`,
		make([]byte, 32), make([]byte, 32), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name   string
		digest domain.UploadContentDigest
		wantID uint64
	}{
		{"size matches exact duplicate key", domain.UploadContentDigest{SizeBytes: domain.MaxSingleUploadSizeBytes}, 29},
		{"zero bytes and GC metadata", domain.UploadContentDigest{}, 30},
		{"unready object", domain.UploadContentDigest{SizeBytes: 1}, 0},
		{"foreign object", domain.UploadContentDigest{SizeBytes: 2}, 0},
		{"different size", domain.UploadContentDigest{SizeBytes: 3}, 0},
		{"different digest", domain.UploadContentDigest{SHA256: [32]byte{0xff}}, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var want *domain.StoredObject
			if scenario.wantID != 0 {
				expected, err := store.FindUserStoredObject(ctx, 7, scenario.wantID)
				if err != nil {
					t.Fatal(err)
				}
				want = expected
			}
			err := store.withActiveUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				object, err := txStore.FindReusableUploadObject(txContext, scenario.digest)
				if err != nil || !reflect.DeepEqual(object, want) {
					t.Fatal("真实 MySQL 查询未得到预期候选", err)
				}
				if object == nil {
					return nil
				}
				// 另一连接立即尝试取同一行的锁，验证查询持有行锁。
				contender, err := pool.BeginTx(txContext, nil)
				if err != nil {
					return err
				}
				defer contender.Rollback()
				var id uint64
				err = contender.QueryRowContext(txContext, "SELECT id FROM stored_objects WHERE id=? FOR UPDATE NOWAIT", object.ID).Scan(&id)
				var lockError *mysql.MySQLError
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					t.Fatal("命中后未保持对象行锁", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if want != nil {
				after, err := store.FindUserStoredObject(ctx, 7, want.ID)
				if err != nil || !reflect.DeepEqual(after, want) {
					t.Fatal("复用查询改变了记录", err)
				}
			}
		})
	}
}
