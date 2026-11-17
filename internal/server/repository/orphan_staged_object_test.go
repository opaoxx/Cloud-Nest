package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// objectReferenceRows 用单列主键表达有效引用是否存在。
type objectReferenceRows struct{ *idempotencyRecordRows }

func (*objectReferenceRows) Columns() []string { return []string{"id"} }

func TestMarkUserStagedObjectOrphanedResults(t *testing.T) {
	fault := errors.New("private orphan failure")
	for _, mode := range []string{"staging", "orphaned without grace", "already orphaned", "longer grace retained", "zero bytes", "maximum object and size", "missing", "query failure", "verifying", "ready", "deleting", "deleted", "staging GC data", "short existing grace", "file reference", "task reference", "file query failure", "task query failure", "write failure", "zero affected", "multiple affected", "missing readback", "readback failure", "wrong status readback", "wrong grace readback", "wrong key readback", "outer failure", "commit failure"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 复用完整对象夹具，保留摘要、位置及诊断，准备暂存阶段。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			want, values := storedObjectReadFixture()
			want.Status, want.DedupHash, want.UnreferencedAt, want.GCAfter = domain.StoredObjectStatusStaging, nil, nil, nil
			values[8], values[9], values[12], values[13] = "STAGING", nil, nil, nil
			stamp := want.CreatedAt.Add(time.Hour)
			gcAfter := stamp.Add(7 * 24 * time.Hour)
			var wantError error
			script := &loginTransactionScript{}
			switch mode {
			case "staging":
			case "orphaned without grace":
				want.Status, values[8] = domain.StoredObjectStatusOrphaned, "ORPHANED"
			case "already orphaned", "longer grace retained", "short existing grace":
				if mode == "longer grace retained" {
					gcAfter = gcAfter.Add(24 * time.Hour)
				}
				if mode == "short existing grace" {
					gcAfter, wantError = gcAfter.Add(-time.Microsecond), domain.ErrInvalidStoredObjectData
				}
				want.Status, want.UnreferencedAt, want.GCAfter = domain.StoredObjectStatusOrphaned, &stamp, &gcAfter
				values[8], values[12], values[13] = "ORPHANED", stamp, gcAfter
			case "zero bytes":
				want.FileSize, values[6] = 0, "0"
			case "maximum object and size":
				want.ID, want.FileSize, values[0], values[6] = ^uint64(0), ^uint64(0), "18446744073709551615", "18446744073709551615"
			case "missing":
				wantError = domain.ErrStoredObjectNotFound
			case "query failure", "file query failure", "task query failure", "write failure", "readback failure", "outer failure", "commit failure":
				wantError = fault
			case "file reference", "task reference":
				wantError = domain.ErrStoredObjectInUse
			case "verifying", "ready", "deleting", "deleted":
				want.Status = domain.StoredObjectStatus(strings.ToUpper(mode))
				values[8], wantError = string(want.Status), domain.ErrInvalidStoredObjectData
				if mode == "ready" {
					values[9] = values[7]
				}
			case "staging GC data":
				values[12], values[13], wantError = stamp, gcAfter, domain.ErrInvalidStoredObjectData
			default:
				wantError = domain.ErrInvalidStoredObjectData
			}
			store := revokeSessionTestStore(t, actor, script)
			if mode == "commit failure" {
				script.commitError = fault
			}
			writes, reads := 0, 0
			script.objectQuery = func(queryContext context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				expectedSQL := "SELECT " + strings.Join((&storedObjectMappingRows{}).Columns(), ",") + " FROM stored_objects WHERE owner_user_id = ? AND id = ? LIMIT ?"
				if reads != 3 {
					expectedSQL += " FOR UPDATE"
				}
				if strings.ReplaceAll(statement, "`", "") != expectedSQL || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID {
					t.Error("对象完整当前读、普通读取或归属范围错误")
				}
				if deadline, exists := queryContext.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("孤立标记缺少查询期限")
				}
				if mode == "query failure" || (reads == 2 && mode == "readback failure") {
					return nil, fault
				}
				result := append([]driver.Value(nil), values...)
				if mode == "missing" || (reads == 2 && mode == "missing readback") {
					result = nil
				}
				if writes > 0 && result != nil {
					result[8], result[12], result[13], result[16] = "ORPHANED", stamp, gcAfter, stamp
					switch mode {
					case "wrong status readback":
						result[8] = "STAGING"
					case "wrong grace readback":
						result[13] = stamp.Add(6 * 24 * time.Hour)
					case "wrong key readback":
						result[4] = "changed-key"
					}
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.entryQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				// 2. 引用检查使用当前读，覆盖回收站与清理中的逻辑内容。
				if strings.ReplaceAll(statement, "`", "") != "SELECT id FROM file_entries WHERE user_id = ? AND object_id = ? AND status <> 'PURGED' LIMIT ? FOR UPDATE" || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID {
					t.Error("文件有效引用当前读错误")
				}
				if mode == "file query failure" {
					return nil, fault
				}
				var result []driver.Value
				if mode == "file reference" {
					result = []driver.Value{"31"}
				}
				return &objectReferenceRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.uploadQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.ReplaceAll(statement, "`", "") != "SELECT id FROM upload_tasks WHERE user_id = ? AND status NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED') AND (staged_object_id = ? OR published_object_id = ?) LIMIT ? FOR UPDATE" || len(arguments) != 4 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID || arguments[2].Value != want.ID {
					t.Error("任务有效引用当前读错误")
				}
				if mode == "task query failure" {
					return nil, fault
				}
				var result []driver.Value
				if mode == "task reference" {
					result = []driver.Value{"19"}
				}
				return &objectReferenceRows{&idempotencyRecordRows{values: result}}, nil
			}
			script.orphanStagedObject = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				expectedSQL := "UPDATE stored_objects SET status = 'ORPHANED', unreferenced_at = UTC_TIMESTAMP(6), gc_after = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 7 DAY), updated_at = UTC_TIMESTAMP(6) WHERE owner_user_id = ? AND id = ? AND status = ? AND unreferenced_at IS NULL AND gc_after IS NULL"
				if statement != expectedSQL || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID || arguments[2].Value != string(want.Status) {
					t.Error("孤立对象条件写入或数据库时间表达式错误")
				}
				if mode == "write failure" {
					return nil, fault
				}
				if mode == "zero affected" {
					return driver.RowsAffected(0), nil
				}
				if mode == "multiple affected" {
					return driver.RowsAffected(2), nil
				}
				return driver.RowsAffected(1), nil
			}
			// 3. 交付完整独立对象，外层失败继续沿事务回滚。
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				actual, err := txStore.MarkUserStagedObjectOrphaned(txContext, want.ID)
				if err != nil {
					if actual != nil {
						t.Error("孤立标记失败仍返回对象")
					}
					return err
				}
				expected := want
				if writes > 0 {
					expected.Status, expected.UnreferencedAt, expected.GCAfter, expected.UpdatedAt = domain.StoredObjectStatusOrphaned, &stamp, &gcAfter, stamp
				}
				if !reflect.DeepEqual(actual, &expected) {
					t.Error("孤立标记或复用结果丢失原事实")
				}
				if writes > 0 {
					actual.VerifiedSHA256[0] ^= 0xff
					again, err := txStore.FindUserStoredObject(txContext, actor.UserID, want.ID)
					if err != nil || !reflect.DeepEqual(again, &expected) {
						t.Error("孤立对象副本或普通读取被污染", err)
					}
				}
				if mode == "outer failure" {
					return fault
				}
				return nil
			})
			if !errors.Is(err, wantError) || (wantError != nil && mode != "commit failure" && strings.Contains(script.sequence(), "commit")) {
				t.Fatal("孤立标记事务结果错误", err, script.sequence())
			}
			if (mode == "already orphaned" || mode == "longer grace retained") && writes != 0 {
				t.Fatal("重复标记重置了宽限期")
			}
		})
	}
}

func TestMarkUserStagedObjectOrphanedBoundary(t *testing.T) {
	for _, mode := range []string{"ordinary store", "bound without transaction", "login transaction", "nil context", "zero object", "wrong user", "zero device", "zero session", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				objectID := uint64(23)
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "zero object":
					objectID = 0
				case "wrong user":
					txStore.loginUserID, wantError = 8, domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID, wantError = 0, domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID, wantError = 0, domain.ErrInvalidSessionData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				}
				object, err := txStore.MarkUserStagedObjectOrphaned(ctx, objectID)
				if object != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Error("无效孤立标记调用未拒绝", err)
				}
				return err
			}
			var err error
			switch mode {
			case "ordinary store":
				err = action(context.Background(), store)
			case "bound without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: 7, authenticatedActor: &actor})
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, action)
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Contains(script.sequence(), "object") || strings.Contains(script.sequence(), "entry") || strings.Contains(script.sequence(), "upload") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效调用进入对象访问或提交", err, script.sequence())
			}
		})
	}
}

func TestMarkUserStagedObjectOrphanedCancellation(t *testing.T) {
	for _, phase := range []string{"object deadline", "file deadline", "task deadline", "file cancelled", "task cancelled", "idempotent cancelled", "write cancelled", "write failure cancelled", "readback cancelled"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在对象、引用、写入及复用阶段取消，结果须为空。
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			want, values := storedObjectReadFixture()
			values[8], values[9], values[12], values[13] = "STAGING", nil, nil, nil
			if phase == "idempotent cancelled" {
				values[8], values[12], values[13] = "ORPHANED", *want.UnreferencedAt, *want.GCAfter
			}
			reads := 0
			script.objectQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				reads++
				if phase == "object deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if reads == 2 {
					cancel()
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			for _, reference := range []string{"file", "task"} {
				query := func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
					if phase == reference+" deadline" {
						<-queryContext.Done()
						return nil, queryContext.Err()
					}
					if phase == reference+" cancelled" || (phase == "idempotent cancelled" && reference == "task") {
						cancel()
					}
					return &objectReferenceRows{&idempotencyRecordRows{}}, nil
				}
				if reference == "file" {
					script.entryQuery = query
				} else {
					script.uploadQuery = query
				}
			}
			script.orphanStagedObject = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
				if phase == "write cancelled" || phase == "write failure cancelled" {
					cancel()
				}
				if phase == "write failure cancelled" {
					return nil, errors.New("private orphan cancellation")
				}
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				object, err := txStore.MarkUserStagedObjectOrphaned(txContext, 23)
				if object != nil {
					t.Error("取消后返回非空孤立对象")
				}
				return err
			})
			// 2. 原因穿透，外层事务回滚完成。
			wantError := error(context.Canceled)
			if strings.HasSuffix(phase, "deadline") {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("孤立标记取消结果错误", err, script.sequence())
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("孤立标记取消后未回滚")
			}
		})
	}
}
