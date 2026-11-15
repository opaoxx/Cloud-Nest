package repository

import (
	"bytes"
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

func renameEntryFixture() (domain.Actor, domain.FileEntry, domain.EntryNameMetadata, []byte) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	parentID := uint64(19)
	stamp := time.Date(2026, 11, 16, 1, 30, 0, 123456000, time.UTC)
	target := domain.FileEntry{ID: 23, UserID: 7, ParentID: &parentID, Name: "旧名.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp}
	extension, category := "pdf", "DOCUMENT"
	return actor, target, domain.EntryNameMetadata{Name: "新名.PDF", Extension: &extension, Category: &category}, []byte("新名.pdf")
}

func TestRenameUserFileEntryResults(t *testing.T) {
	failure := errors.New("private entry fault")
	for _, testCase := range []struct {
		name string
		want error
	}{
		{"file", nil}, {"directory", nil}, {"same name", nil}, {"maximum target", nil},
		{"missing scope", domain.ErrFileEntryNotFound}, {"scope fault", failure}, {"wrong scope", domain.ErrInvalidFileEntryData},
		{"missing locked target", domain.ErrFileEntryNotFound}, {"lock fault", failure}, {"wrong owner", domain.ErrInvalidFileEntryData},
		{"trashed", domain.ErrFileEntryNotFound}, {"purging", domain.ErrFileEntryNotFound}, {"purged", domain.ErrFileEntryNotFound},
		{"root", domain.ErrRootEntryRename}, {"zero version", domain.ErrInvalidFileEntryData}, {"zero time", domain.ErrInvalidFileEntryData},
		{"invalid parent", domain.ErrInvalidFileEntryData}, {"unknown type", domain.ErrInvalidFileEntryData},
		{"file missing category", domain.ErrInvalidFileEntryData}, {"file unknown category", domain.ErrInvalidFileEntryData},
		{"directory file metadata", domain.ErrInvalidFileEntryData}, {"directory nonzero size", domain.ErrInvalidFileEntryData},
		{"current version conflict", domain.ErrEntryRevisionConflict}, {"stale same name", domain.ErrEntryRevisionConflict},
		{"maximum version update", domain.ErrEntryRevisionExhausted}, {"maximum version same name", nil},
		{"duplicate", domain.ErrEntryNameConflict}, {"wrapped duplicate", domain.ErrEntryNameConflict}, {"write fault", failure},
		{"zero rows", domain.ErrInvalidFileEntryData}, {"multiple rows", domain.ErrInvalidFileEntryData},
		{"missing read back", domain.ErrInvalidFileEntryData}, {"read fault", failure}, {"bad read back", domain.ErrInvalidFileEntryData},
		{"outer callback", failure}, {"commit fault", failure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 复用真实事务脚本，分别提供归属、当前锁定记录及写后回读。
			actor, target, metadata, key := renameEntryFixture()
			expectedRevision := uint64(3)
			switch testCase.name {
			case "directory", "directory nonzero size":
				target.EntryType, target.FileSize = "DIR", 0
				metadata.Extension, metadata.Category = nil, nil
				if testCase.name == "directory nonzero size" {
					target.FileSize = 1
				}
			case "same name", "stale same name":
				metadata.Name = target.Name
			case "maximum target":
				target.ID = ^uint64(0)
			case "wrong owner":
				target.UserID = 99
			case "trashed":
				target.Status = "TRASHED"
			case "purging":
				target.Status = "PURGING"
			case "purged":
				target.Status = "PURGED"
			case "root":
				target.ParentID, target.Name, target.EntryType, target.FileSize = nil, "", "DIR", 0
			case "zero version":
				target.Revision = 0
			case "zero time":
				target.CreatedAt = time.Time{}
			case "invalid parent":
				*target.ParentID = target.ID
			case "unknown type":
				target.EntryType = "LINK"
			case "file missing category":
				metadata.Category = nil
			case "file unknown category":
				*metadata.Category = "private-kind"
			case "directory file metadata":
				target.EntryType, target.FileSize = "DIR", 0
			case "current version conflict":
				target.Revision = 4
			case "maximum version update", "maximum version same name":
				target.Revision, expectedRevision = ^uint64(0), ^uint64(0)
				if testCase.name == "maximum version same name" {
					metadata.Name = target.Name
				}
			}
			if testCase.name == "stale same name" {
				expectedRevision = 2
			}
			script := &loginTransactionScript{}
			if testCase.name == "commit fault" {
				script.commitError = failure
			}
			store := revokeSessionTestStore(t, actor, script)
			reads, writes := 0, 0
			sameName := target.Name == metadata.Name
			want := target
			if !sameName && target.Revision != ^uint64(0) {
				want.Name, want.Revision = metadata.Name, target.Revision+1
				want.ModifiedAt, want.UpdatedAt = target.ModifiedAt.Add(time.Second), target.UpdatedAt.Add(time.Second)
			}
			script.entryQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				statement = strings.ReplaceAll(statement, "`", "")
				if len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID {
					t.Error("改名读取未绑定用户及目标")
				}
				if reads == 1 {
					if statement != "SELECT id,user_id FROM file_entries WHERE user_id = ? AND id = ? LIMIT ?" {
						t.Error("归属查询应只读两列")
					}
					if testCase.name == "scope fault" {
						return nil, failure
					}
					if testCase.name == "missing scope" {
						return &renameDeviceScopeRows{}, nil
					}
					owner := actor.UserID
					if testCase.name == "wrong scope" {
						owner = 99
					}
					return &renameDeviceScopeRows{values: []driver.Value{strconv.FormatUint(target.ID, 10), strconv.FormatUint(owner, 10)}}, nil
				}
				if reads == 2 {
					if !strings.HasSuffix(statement, "LIMIT ? FOR UPDATE") {
						t.Error("未锁定读取当前目录项")
					}
					if testCase.name == "lock fault" {
						return nil, failure
					}
					if testCase.name == "missing locked target" {
						return &fileEntryRows{}, nil
					}
					return &fileEntryRows{values: directoryEntryValues(target)}, nil
				}
				if strings.Contains(statement, "FOR UPDATE") {
					t.Error("写后回读意外重复加锁")
				}
				if testCase.name == "read fault" {
					return nil, failure
				}
				if testCase.name == "missing read back" {
					return &fileEntryRows{}, nil
				}
				result := want
				if testCase.name == "bad read back" {
					result.Name = "private-wrong-name"
				}
				return &fileEntryRows{values: directoryEntryValues(result)}, nil
			}
			script.renameEntry = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				wantStatement := "UPDATE file_entries SET name = ?, name_key = ?, extension = ?, category = ?, revision = revision + 1, modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND status = 'ACTIVE' AND revision = ?"
				if statement != wantStatement || len(arguments) != 7 {
					t.Fatal("名称条件更新列或绑定条件错误")
				}
				var extension, category any
				if metadata.Extension != nil {
					extension = *metadata.Extension
				}
				if metadata.Category != nil {
					category = *metadata.Category
				}
				values := []any{metadata.Name, key, extension, category, actor.UserID, target.ID, expectedRevision}
				for index, argument := range arguments {
					if !reflect.DeepEqual(argument.Value, values[index]) {
						t.Error("改名绑定值或 NULL 语义错误", index)
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("更新未服从事务期限")
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
				prepared, err = txStore.RenameUserFileEntry(txContext, target.ID, expectedRevision, metadata, key)
				if err != nil {
					return err
				}
				if testCase.name == "outer callback" {
					return failure
				}
				return nil
			})
			// 2. 确认提交/回滚边界，并检查无变化与版本冲突的处理顺序。
			if testCase.want == nil {
				if err != nil || !reflect.DeepEqual(prepared, &want) || !strings.HasSuffix(script.sequence(), "commit") {
					t.Fatal("改名结果或提交错误", err, script.sequence())
				}
				if (sameName && (writes != 0 || reads != 2)) || (!sameName && (writes != 1 || reads != 3)) {
					t.Fatal("无变化或实际更新的执行次数错误")
				}
				return
			}
			if !errors.Is(err, testCase.want) {
				t.Fatal("改名失败未保留预期错误", err)
			}
			if testCase.name != "outer callback" && testCase.name != "commit fault" && prepared != nil {
				t.Fatal("操作失败返回部分结果")
			}
			if errors.Is(err, domain.ErrEntryRevisionConflict) {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != target.Revision || writes != 0 {
					t.Fatal("当前版本冲突数据或写入次数错误")
				}
			}
			if errors.Is(err, domain.ErrEntryNameConflict) && strings.Contains(err.Error(), "private") {
				t.Fatal("名称冲突包含数据库原文")
			}
			ending := "rollback"
			if testCase.name == "commit fault" {
				ending = "commit"
			}
			if !strings.HasSuffix(script.sequence(), ending) {
				t.Fatal("失败事务结束方式错误", script.sequence())
			}
		})
	}
}

func TestRenameUserFileEntryInputAndCopies(t *testing.T) {
	actor, target, metadata, key := renameEntryFixture()
	script := &loginTransactionScript{}
	store := revokeSessionTestStore(t, actor, script)
	for _, candidate := range []*Store{store, {database: store.database, loginUserID: 7, authenticatedActor: &actor}} {
		if entry, err := candidate.RenameUserFileEntry(context.Background(), target.ID, 3, metadata, key); entry != nil || err == nil || script.sequence() != "" {
			t.Fatal("普通 Store 获准执行改名", err)
		}
	}
	err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
		cancelled, cancel := context.WithCancel(txContext)
		cancel()
		for _, mode := range []string{"nil context", "zero target", "zero revision", "empty name", "invalid name encoding", "missing key", "long key", "invalid extension encoding", "long extension", "cancelled"} {
			ctx, entryID, revision, values, bytesKey := txContext, target.ID, uint64(3), metadata, key
			switch mode {
			case "nil context":
				ctx = nil
			case "zero target":
				entryID = 0
			case "zero revision":
				revision = 0
			case "empty name":
				values.Name = ""
			case "invalid name encoding":
				values.Name = "private\xff"
			case "missing key":
				bytesKey = nil
			case "long key":
				bytesKey = bytes.Repeat([]byte("a"), 769)
			case "invalid extension encoding":
				value := "\xff"
				values.Extension = &value
			case "long extension":
				value := strings.Repeat("x", 33)
				values.Extension = &value
			case "cancelled":
				ctx = cancelled
			}
			if entry, err := txStore.RenameUserFileEntry(ctx, entryID, revision, values, bytesKey); entry != nil || err == nil {
				t.Error("无效输入未拒绝", mode, err)
			}
		}
		return nil
	})
	if err != nil || script.sequence() != "begin,lock,device,session,commit" {
		t.Fatal("无效输入产生额外查询或写入", err, script.sequence())
	}

	// 3. 查询阶段改动调用方缓冲区，写入仍使用进入方法时复制的字段。
	actor, target, metadata, key = renameEntryFixture()
	script = &loginTransactionScript{}
	store = revokeSessionTestStore(t, actor, script)
	reads := 0
	script.entryQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		reads++
		if reads == 1 {
			key[0] = 'x'
			*metadata.Extension = "changed"
			*metadata.Category = "OTHER"
			return &renameDeviceScopeRows{values: []driver.Value{"23", "7"}}, nil
		}
		if reads == 2 {
			return &fileEntryRows{values: directoryEntryValues(target)}, nil
		}
		result := target
		result.Name = "新名.PDF"
		result.Revision = 4
		return &fileEntryRows{values: directoryEntryValues(result)}, nil
	}
	script.renameEntry = func(_ context.Context, _ string, arguments []driver.NamedValue) (driver.Result, error) {
		if !bytes.Equal(arguments[1].Value.([]byte), []byte("新名.pdf")) || arguments[2].Value != "pdf" || arguments[3].Value != "DOCUMENT" {
			t.Error("更新参数共享原始缓冲区")
		}
		return driver.RowsAffected(1), nil
	}
	err = store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.RenameUserFileEntry(txContext, target.ID, 3, metadata, key)
		return err
	})
	if err != nil {
		t.Fatal("输入副本验证失败", err)
	}
}

func TestRenameUserFileEntryCancellation(t *testing.T) {
	for _, phase := range []string{"scope", "lock", "update", "read back"} {
		t.Run(phase, func(t *testing.T) {
			actor, target, metadata, key := renameEntryFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			reads := 0
			script.entryQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				reads++
				if (phase == "scope" && reads == 1) || (phase == "lock" && reads == 2) || (phase == "read back" && reads == 3) {
					cancel()
				}
				if reads == 1 {
					return &renameDeviceScopeRows{values: []driver.Value{"23", "7"}}, nil
				}
				result := target
				if reads == 3 {
					result.Name = metadata.Name
					result.Revision++
				}
				return &fileEntryRows{values: directoryEntryValues(result)}, nil
			}
			script.renameEntry = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
				if phase == "update" {
					cancel()
				}
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				entry, err := txStore.RenameUserFileEntry(txContext, target.ID, 3, metadata, key)
				if entry != nil {
					t.Error("取消返回了改名结果")
				}
				return err
			})
			if !errors.Is(err, context.Canceled) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("取消原因或回滚边界错误", err, script.sequence())
			}
		})
	}
}
