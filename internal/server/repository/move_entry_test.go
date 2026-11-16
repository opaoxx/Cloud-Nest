package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func moveEntryFixture() (domain.Actor, domain.FileEntry, domain.FileEntry) {
	actor, source, _, _ := renameEntryFixture()
	rootID := uint64(11)
	target := source
	target.ID, target.ParentID, target.Name, target.EntryType, target.FileSize, target.Revision = 31, &rootID, "目的地", "DIR", 0, 1
	return actor, source, target
}

func TestMoveUserFileEntryResults(t *testing.T) {
	failure := errors.New("private move fault")
	for _, testCase := range []struct {
		name string
		want error
	}{
		{"file", nil}, {"directory", nil}, {"root destination", nil}, {"same parent", nil}, {"maximum source", nil}, {"maximum destination", nil}, {"maximum revision increment", nil}, {"maximum revision same parent", nil},
		{"missing source scope", domain.ErrFileEntryNotFound}, {"missing target scope", domain.ErrFileEntryNotFound}, {"scope fault", failure}, {"wrong scope", domain.ErrInvalidFileEntryData},
		{"missing locked source", domain.ErrFileEntryNotFound}, {"missing locked target", domain.ErrFileEntryNotFound}, {"lock fault", failure}, {"wrong locked owner", domain.ErrInvalidFileEntryData},
		{"hidden source", domain.ErrFileEntryNotFound}, {"hidden target", domain.ErrFileEntryNotFound}, {"root source", domain.ErrRootEntryMove}, {"target file", domain.ErrFileEntryNotDirectory},
		{"directory itself", domain.ErrEntryMoveIntoSubtree}, {"file itself", domain.ErrFileEntryNotDirectory}, {"zero revision", domain.ErrInvalidFileEntryData}, {"zero time", domain.ErrInvalidFileEntryData},
		{"self parent", domain.ErrInvalidFileEntryData}, {"unknown type", domain.ErrInvalidFileEntryData}, {"invalid utf8", domain.ErrInvalidFileEntryData}, {"invalid target root", domain.ErrInvalidFileEntryData}, {"directory size", domain.ErrInvalidFileEntryData},
		{"revision conflict", domain.ErrEntryRevisionConflict}, {"stale same parent", domain.ErrEntryRevisionConflict}, {"maximum revision change", domain.ErrEntryRevisionExhausted},
		{"duplicate", domain.ErrEntryNameConflict}, {"wrapped duplicate", domain.ErrEntryNameConflict}, {"write fault", failure}, {"zero rows", domain.ErrInvalidFileEntryData}, {"multiple rows", domain.ErrInvalidFileEntryData},
		{"missing readback", domain.ErrInvalidFileEntryData}, {"readback fault", failure}, {"wrong readback", domain.ErrInvalidFileEntryData}, {"outer callback", failure}, {"commit fault", failure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, source, target := moveEntryFixture()
			expectedRevision := uint64(3)
			switch testCase.name {
			case "directory":
				source.EntryType, source.FileSize = "DIR", 0
			case "root destination":
				target.ID, target.ParentID, target.Name = 11, nil, ""
			case "same parent", "stale same parent", "maximum revision same parent":
				target.ID = *source.ParentID
			case "maximum source":
				source.ID = ^uint64(0)
			case "maximum destination":
				target.ID = ^uint64(0)
			case "root source":
				source.ParentID, source.Name, source.EntryType, source.FileSize = nil, "", "DIR", 0
			case "target file":
				target.EntryType, target.FileSize = "FILE", 1
			case "directory itself":
				source.EntryType, source.FileSize = "DIR", 0
				target = source
			case "file itself":
				target = source
			case "zero revision":
				source.Revision = 0
			case "zero time":
				target.UpdatedAt = time.Time{}
			case "self parent":
				source.ParentID = &source.ID
			case "unknown type":
				source.EntryType = "LINK"
			case "invalid utf8":
				source.Name = "private\xff"
			case "invalid target root":
				target.ParentID = nil
			case "directory size":
				target.FileSize = 1
			case "revision conflict":
				source.Revision = 4
			case "maximum revision increment":
				source.Revision, expectedRevision = ^uint64(0)-1, ^uint64(0)-1
			case "maximum revision change":
				source.Revision, expectedRevision = ^uint64(0), ^uint64(0)
			case "hidden source":
				source.Status = "TRASHED"
			case "hidden target":
				target.Status = "PURGING"
			}
			if testCase.name == "stale same parent" {
				source.Revision = 4
			}
			if testCase.name == "maximum revision same parent" {
				source.Revision, expectedRevision = ^uint64(0), ^uint64(0)
			}
			entries := map[uint64]domain.FileEntry{source.ID: source, target.ID: target}
			ids := []uint64{source.ID}
			if target.ID != source.ID {
				ids = append(ids, target.ID)
				if ids[0] > ids[1] {
					ids[0], ids[1] = ids[1], ids[0]
				}
			}
			want := source
			sameParent := source.ParentID != nil && *source.ParentID == target.ID
			if !sameParent && source.Revision != ^uint64(0) {
				parent := target.ID
				want.ParentID = &parent
				want.Revision++
				want.ModifiedAt = source.ModifiedAt.Add(time.Second)
				want.UpdatedAt = source.UpdatedAt.Add(time.Second)
			}
			script := &loginTransactionScript{status: "ACTIVE"}
			if testCase.name == "commit fault" {
				script.commitError = failure
			}
			store := revokeSessionTestStore(t, actor, script)
			var scopedIDs, lockedIDs []uint64
			writes, reads := 0, 0
			script.entryQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if len(arguments) != 3 || arguments[0].Value != actor.UserID {
					t.Fatal("移动查询参数错误")
				}
				id := arguments[1].Value.(uint64)
				normalized := strings.ReplaceAll(statement, "`", "")
				record := entries[id]
				if strings.HasPrefix(normalized, "SELECT id,user_id FROM") {
					scopedIDs = append(scopedIDs, id)
					if len(lockedIDs) != 0 {
						t.Error("锁定前未完成全部范围定位")
					}
					if testCase.name == "scope fault" {
						return nil, failure
					}
					if (testCase.name == "missing source scope" && id == source.ID) || (testCase.name == "missing target scope" && id == target.ID) {
						return &renameDeviceScopeRows{}, nil
					}
					owner := actor.UserID
					if testCase.name == "wrong scope" {
						owner = 99
					}
					return &renameDeviceScopeRows{values: []driver.Value{strconv.FormatUint(id, 10), strconv.FormatUint(owner, 10)}}, nil
				}
				if strings.HasSuffix(normalized, "FOR UPDATE") {
					lockedIDs = append(lockedIDs, id)
					if len(lockedIDs) > 1 && lockedIDs[len(lockedIDs)-2] >= id {
						t.Error("记录加锁未按主键升序")
					}
					if testCase.name == "lock fault" {
						return nil, failure
					}
					if (testCase.name == "missing locked source" && id == source.ID) || (testCase.name == "missing locked target" && id == target.ID) {
						return &fileEntryRows{}, nil
					}
					if testCase.name == "wrong locked owner" {
						record.UserID = 99
					}
					return &fileEntryRows{values: directoryEntryValues(record)}, nil
				}
				if testCase.name == "missing readback" {
					return &fileEntryRows{}, nil
				}
				if testCase.name == "readback fault" {
					return nil, failure
				}
				readback := want
				if testCase.name == "wrong readback" {
					parent := uint64(99)
					readback.ParentID = &parent
				}
				return &fileEntryRows{values: directoryEntryValues(readback)}, nil
			}
			script.moveEntry = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				wantStatement := "UPDATE file_entries SET parent_id = ?, revision = revision + 1, modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND status = 'ACTIVE' AND revision = ?"
				if statement != wantStatement || len(arguments) != 4 {
					t.Fatal("移动条件更新结构错误")
				}
				for index, value := range []any{target.ID, actor.UserID, source.ID, expectedRevision} {
					if !reflect.DeepEqual(arguments[index].Value, value) {
						t.Error("移动绑定值错误", index)
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("移动查询期限错误")
				}
				switch testCase.name {
				case "duplicate":
					return nil, &mysql.MySQLError{Number: 1062, Message: "private duplicate"}
				case "wrapped duplicate":
					return nil, fmt.Errorf("driver: %w", &mysql.MySQLError{Number: 1062, Message: "private duplicate"})
				case "write fault":
					return nil, failure
				case "zero rows":
					return driver.RowsAffected(0), nil
				case "multiple rows":
					return driver.RowsAffected(2), nil
				}
				return driver.RowsAffected(1), nil
			}
			var prepared *domain.FileEntry
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				var err error
				prepared, err = txStore.MoveUserFileEntry(txContext, source.ID, expectedRevision, target.ID)
				if err != nil {
					return err
				}
				if testCase.name == "outer callback" {
					return failure
				}
				return nil
			})
			if testCase.want == nil {
				if err != nil || !reflect.DeepEqual(prepared, &want) || !strings.HasSuffix(script.sequence(), "commit") || !reflect.DeepEqual(scopedIDs, ids) || !reflect.DeepEqual(lockedIDs, ids) {
					t.Fatal("移动提交、事实或锁顺序错误", err, script.sequence())
				}
				if sameParent {
					if writes != 0 || reads != len(ids)*2 {
						t.Fatal("同位置匹配执行次数错误")
					}
				} else if writes != 1 || reads != len(ids)*2+1 {
					t.Fatal("实际移动执行次数错误")
				}
			} else {
				if !errors.Is(err, testCase.want) {
					t.Fatal("移动失败错误链错误", err)
				}
				if testCase.name != "outer callback" && testCase.name != "commit fault" && prepared != nil {
					t.Fatal("方法失败返回了结果")
				}
				if testCase.name != "commit fault" && !strings.HasSuffix(script.sequence(), "rollback") {
					t.Fatal("失败未回滚", script.sequence())
				}
				if errors.Is(err, domain.ErrEntryRevisionConflict) {
					var conflict *domain.EntryRevisionConflictError
					if !errors.As(err, &conflict) || conflict.CurrentRevision != 4 {
						t.Fatal("当前版本丢失")
					}
				}
			}
		})
	}
}

func TestMoveUserFileEntryInputsAndCancellation(t *testing.T) {
	for _, mode := range []string{"ordinary store", "bound nontransaction store", "nil context", "zero source", "zero revision", "zero destination", "missing actor", "wrong user", "zero device", "zero session", "cancelled input", "cancel scope", "cancel lock", "cancel update", "cancel readback"} {
		t.Run(mode, func(t *testing.T) {
			actor, source, target := moveEntryFixture()
			script := &loginTransactionScript{status: "ACTIVE"}
			store := revokeSessionTestStore(t, actor, script)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "ordinary store" || mode == "bound nontransaction store" {
				if mode == "bound nontransaction store" {
					store.loginUserID = actor.UserID
					store.authenticatedActor = &actor
				}
				if entry, err := store.MoveUserFileEntry(ctx, source.ID, 3, target.ID); entry != nil || err == nil {
					t.Fatal("普通连接被用于移动")
				}
				return
			}
			queries := 0
			script.entryQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				id := arguments[1].Value.(uint64)
				normalized := strings.ReplaceAll(statement, "`", "")
				if strings.HasPrefix(normalized, "SELECT id,user_id FROM ") {
					if mode == "cancel scope" {
						cancel()
					}
					return &renameDeviceScopeRows{values: []driver.Value{strconv.FormatUint(id, 10), int64(7)}}, nil
				}
				record := source
				if id == target.ID {
					record = target
				}
				if strings.HasSuffix(statement, "FOR UPDATE") {
					if mode == "cancel lock" {
						cancel()
					}
				} else {
					parent := target.ID
					record.ParentID = &parent
					record.Revision++
					if mode == "cancel readback" {
						cancel()
					}
				}
				return &fileEntryRows{values: directoryEntryValues(record)}, nil
			}
			script.moveEntry = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
				if mode == "cancel update" {
					cancel()
				}
				return driver.RowsAffected(1), nil
			}
			var result *domain.FileEntry
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				var callContext context.Context = txContext
				sourceID, revision, parent := source.ID, uint64(3), target.ID
				switch mode {
				case "nil context":
					callContext = nil
				case "zero source":
					sourceID = 0
				case "zero revision":
					revision = 0
				case "zero destination":
					parent = 0
				case "missing actor":
					txStore.authenticatedActor = nil
				case "wrong user":
					txStore.loginUserID++
				case "zero device":
					txStore.authenticatedActor.DeviceID = 0
				case "zero session":
					txStore.authenticatedActor.SessionID = 0
				case "cancelled input":
					cancel()
				}
				var err error
				result, err = txStore.MoveUserFileEntry(callContext, sourceID, revision, parent)
				return err
			})
			if result != nil || err == nil {
				t.Fatal("输入或取消未拒绝", err)
			}
			if strings.HasPrefix(mode, "cancel") {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("取消原因丢失", err)
				}
			} else if queries != 0 {
				t.Fatal("非法输入执行了目录项查询")
			}
		})
	}
}
