package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClearPublishedObjectGCTimerIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects
		(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at)
		VALUES(30,7,'MINIO','fixture-bucket','gc-timer-target',0,UNHEX(REPEAT('00',32)),'READY',UTC_TIMESTAMP(6)),
		      (32,8,'MINIO','fixture-bucket','foreign-gc-timer-target',0,UNHEX(REPEAT('00',32)),'READY',UTC_TIMESTAMP(6)),
		      (33,7,'MINIO','fixture-bucket','rollback-gc-timer-target',1,UNHEX(REPEAT('00',32)),'READY',UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries
		(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES(40,8,14,'foreign file',CAST('foreign file' AS BINARY),'FILE','OTHER',32,0)`); err != nil {
		t.Fatal(err)
	}
	rollbackFixture := errors.New("fixture rolls back reference and timer")
	for _, scenario := range []struct {
		name              string
		objectID, entryID uint64
		timer             bool
		insertReference   bool
		expiredTimer      bool
		changeSQL         string
		wantError         error
	}{
		{name: "existing reference and no timer", objectID: 29, entryID: 31},
		{name: "timed object gets first reference", objectID: 30, entryID: 41, timer: true, insertReference: true},
		{name: "elapsed timer on READY object rolls back with new reference", objectID: 33, entryID: 42, timer: true, insertReference: true, expiredTimer: true, wantError: rollbackFixture},
		{name: "unrelated file", objectID: 33, entryID: 31, timer: true, wantError: domain.ErrFileEntryNotFound},
		{name: "foreign file and object", objectID: 32, entryID: 40, timer: true, wantError: domain.ErrFileEntryNotFound},
		{name: "trashed reference", objectID: 29, entryID: 31, timer: true, changeSQL: "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=31", wantError: domain.ErrFileEntryNotFound},
		{name: "object already deleting", objectID: 29, entryID: 31, timer: true, changeSQL: "UPDATE stored_objects SET status='DELETING' WHERE id=29", wantError: domain.ErrInvalidStoredObjectData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备已开始计时的对象和目标引用范围。
			ownerID := uint64(7)
			if scenario.objectID == 32 {
				ownerID = 8
			}
			if scenario.timer {
				var now time.Time
				if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
					t.Fatal(err)
				}
				deadline := now.Add(24 * time.Hour)
				if scenario.expiredTimer {
					deadline = now.Add(-24 * time.Hour)
				}
				if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET unreferenced_at=?,gc_after=?,updated_at='2026-11-10 08:00:00.123456' WHERE id=?",
					deadline.Add(-7*24*time.Hour), deadline, scenario.objectID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.FindUserStoredObject(ctx, ownerID, scenario.objectID)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 在同一用户事务中写入测试引用并验证计时清除及重复调用。
			var result *domain.StoredObject
			err = store.withActiveUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				if scenario.insertReference {
					insert := txStore.database.WithContext(txContext).Exec(`INSERT INTO file_entries
						(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
						VALUES(?,7,13,?,?,'FILE','OTHER',?,?)`, scenario.entryID, scenario.name, []byte(strings.ToLower(scenario.name)), before.ID, before.FileSize)
					if insert.Error != nil {
						return insert.Error
					}
				}
				if scenario.changeSQL != "" {
					if err := txStore.database.WithContext(txContext).Exec(scenario.changeSQL).Error; err != nil {
						return err
					}
				}
				var err error
				result, err = txStore.ClearPublishedObjectGCTimer(txContext, scenario.objectID, scenario.entryID)
				if err != nil {
					return err
				}
				expected := *before
				expected.UnreferencedAt, expected.GCAfter = nil, nil
				if scenario.timer {
					if result.UpdatedAt.Equal(before.UpdatedAt) {
						t.Fatal("清除计时未更新对象数据库修改时间")
					}
					expected.UpdatedAt = result.UpdatedAt
				}
				if !reflect.DeepEqual(result, &expected) {
					t.Fatal("清除计时改变了其他对象事实", result)
				}
				repeated, err := txStore.ClearPublishedObjectGCTimer(txContext, scenario.objectID, scenario.entryID)
				if err != nil || !reflect.DeepEqual(repeated, result) {
					t.Fatal("计时已清除时重复调用改变了记录", repeated, err)
				}
				if scenario.wantError == rollbackFixture {
					return rollbackFixture
				}
				return nil
			})
			if !errors.Is(err, scenario.wantError) || scenario.wantError != nil && scenario.wantError != rollbackFixture && result != nil {
				t.Fatal("清除计时事务结果不符", result, err)
			}

			// 3. 核对提交或回滚后的计时，并确认新引用和计时同步生效。
			wantObject := result
			if scenario.wantError != nil {
				wantObject = before
			}
			after, err := store.FindUserStoredObject(ctx, ownerID, scenario.objectID)
			if err != nil || !reflect.DeepEqual(after, wantObject) {
				t.Fatal("事务结束后对象计时错误", after, err)
			}
			if scenario.insertReference {
				entry, err := store.FindUserFileEntry(ctx, 7, scenario.entryID)
				if scenario.wantError == nil && (entry == nil || err != nil) ||
					scenario.wantError != nil && (entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound)) {
					t.Fatal("新文件引用和计时没有一并提交或回滚", entry, err)
				}
			}
		})
	}
}
