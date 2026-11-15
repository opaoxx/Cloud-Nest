package repository

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCreateUserDirectoryResults(t *testing.T) {
	writeFailure := errors.New("private directory insert failure")
	readFailure := errors.New("private directory read failure")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private duplicate name"}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "private foreign key"}
	checkFailure := &mysql.MySQLError{Number: 3819, Message: "private check failure"}
	for _, testCase := range []struct {
		name                                         string
		parentStatus, parentType, resultField        string
		parentMissing, readMissing                   bool
		writeError, readError, outerError, wantError error
		rows                                         int64
	}{
		{name: "null color", rows: 1},
		{name: "green color", rows: 1},
		{name: "empty color preserved", rows: 1},
		{name: "full unsigned generated id", rows: 1},
		{name: "missing parent", parentMissing: true, wantError: domain.ErrFileEntryNotFound},
		{name: "file parent", parentType: "FILE", wantError: domain.ErrFileEntryNotDirectory},
		{name: "trashed parent", parentStatus: "TRASHED", wantError: domain.ErrFileEntryNotFound},
		{name: "purging parent", parentStatus: "PURGING", wantError: domain.ErrFileEntryNotFound},
		{name: "purged parent", parentStatus: "PURGED", wantError: domain.ErrFileEntryNotFound},
		{name: "parent query failure", readError: readFailure, wantError: readFailure},
		{name: "duplicate name", writeError: duplicate, wantError: domain.ErrEntryNameConflict},
		{name: "wrapped duplicate", writeError: fmt.Errorf("driver: %w", duplicate), wantError: domain.ErrEntryNameConflict},
		{name: "foreign key failure", writeError: foreignKey, wantError: foreignKey},
		{name: "check failure", writeError: checkFailure, wantError: checkFailure},
		{name: "insert failure", writeError: writeFailure, wantError: writeFailure},
		{name: "zero affected rows", wantError: domain.ErrInvalidFileEntryData},
		{name: "multiple affected rows", rows: 2, wantError: domain.ErrInvalidFileEntryData},
		{name: "missing read back", rows: 1, readMissing: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "read back failure", rows: 1, readError: readFailure, wantError: readFailure},
		{name: "wrong read back name", rows: 1, resultField: "name", wantError: domain.ErrInvalidFileEntryData},
		{name: "wrong read back type", rows: 1, resultField: "type", wantError: domain.ErrInvalidFileEntryData},
		{name: "wrong read back size", rows: 1, resultField: "size", wantError: domain.ErrInvalidFileEntryData},
		{name: "wrong read back revision", rows: 1, resultField: "revision", wantError: domain.ErrInvalidFileEntryData},
		{name: "outer callback failure", rows: 1, outerError: errors.New("outer directory failure")},
		{name: "commit failure", rows: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 沿用认证事务脚本，准备父目录读取与插入后的目录记录。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			parentID := uint64(19)
			stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
			name := "学习资料"
			key := []byte(name)
			var color *string
			if testCase.name == "green color" || testCase.name == "empty color preserved" {
				value := "green"
				if testCase.name == "empty color preserved" {
					value = ""
				}
				color = &value
			}
			parent := domain.FileEntry{ID: parentID, UserID: actor.UserID, EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp}
			if testCase.parentStatus != "" {
				parent.Status = testCase.parentStatus
			}
			if testCase.parentType != "" {
				parent.EntryType = testCase.parentType
			}
			want := domain.FileEntry{ID: 23, UserID: actor.UserID, ParentID: &parentID, Name: name, EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp}
			if testCase.name == "full unsigned generated id" {
				want.ID = ^uint64(0)
			}
			values := directoryEntryValues(want)
			switch testCase.resultField {
			case "name":
				values[3] = "other"
			case "type":
				values[4] = "FILE"
			case "size":
				values[5] = int64(1)
			case "revision":
				values[6] = int64(2)
			}
			script := &loginTransactionScript{}
			if testCase.name == "commit failure" {
				script.commitError = errors.New("directory commit failure")
			}
			store := revokeSessionTestStore(t, actor, script)
			reads, writes := 0, 0
			script.entryQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				statement = strings.ReplaceAll(statement, "`", "")
				projection := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE "
				if reads == 1 {
					if statement != projection+"user_id = ? AND id = ? LIMIT ?" || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != parentID {
						t.Error("父目录读取未按当前用户和目标 ID 限定")
					}
					if testCase.parentMissing {
						return &fileEntryRows{}, nil
					}
					if testCase.name == "parent query failure" {
						return nil, testCase.readError
					}
					return &fileEntryRows{values: directoryEntryValues(parent)}, nil
				}
				if statement != projection+"user_id = ? AND parent_id = ? AND active_name_key = ? AND status = ? LIMIT ?" || len(arguments) != 5 || arguments[0].Value != actor.UserID || arguments[1].Value != parentID || !bytes.Equal(arguments[2].Value.([]byte), key) {
					t.Error("新目录回读未使用当前用户、父目录及名称唯一键")
				}
				if testCase.readMissing {
					return &fileEntryRows{}, nil
				}
				return &fileEntryRows{values: values}, testCase.readError
			}
			script.insert = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				if strings.ReplaceAll(statement, "`", "") != "INSERT INTO file_entries (user_id,parent_id,name,name_key,entry_type,status,color_key) VALUES (?,?,?,?,?,?,?)" || len(arguments) != 7 {
					t.Fatal("目录 INSERT 字段或占位符错误")
				}
				var expectedColor any
				if color != nil {
					expectedColor = *color
				}
				expected := []any{actor.UserID, parentID, name, key, "DIR", "ACTIVE", expectedColor}
				for index, argument := range arguments {
					if !reflect.DeepEqual(argument.Value, expected[index]) {
						t.Error("目录字段、NULL 语义或绑定值错误")
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("创建目录未服从事务期限")
				}
				// 模型通过唯一键回读自增 ID，此驱动故障可验证该路径。
				return deviceInsertResult{rows: testCase.rows, idError: errors.New("LastInsertId should not be used")}, testCase.writeError
			}

			// 2. 将所有操作错误交回事务入口，提交成功后确认最终结果。
			var prepared *domain.FileEntry
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				var err error
				prepared, err = txStore.CreateUserDirectory(txContext, parentID, name, key, color)
				if err != nil {
					return err
				}
				return testCase.outerError
			})
			failed := testCase.wantError != nil || testCase.outerError != nil || script.commitError != nil
			if !failed {
				if err != nil || !reflect.DeepEqual(prepared, &want) || script.sequence() != "begin,lock,device,session,entry,insert,entry,commit" {
					t.Fatal("目录创建、读回或事务顺序错误", err, script.sequence())
				}
				return
			}
			lastEvent := "rollback"
			if script.commitError != nil {
				lastEvent = "commit"
			}
			if err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) || (testCase.outerError != nil && !errors.Is(err, testCase.outerError)) || (script.commitError != nil && !errors.Is(err, script.commitError)) || !strings.HasSuffix(script.sequence(), lastEvent) {
				t.Fatal("创建失败未保留错误或回滚", err, script.sequence())
			}
			if testCase.wantError != nil && prepared != nil {
				t.Fatal("目录操作失败仍返回部分结果")
			}
			if errors.Is(err, domain.ErrEntryNameConflict) && strings.Contains(err.Error(), "private") {
				t.Fatal("名称冲突回显了数据库原始消息")
			}
			if reads == 1 && writes != 0 && (testCase.parentMissing || testCase.parentStatus != "" || testCase.parentType != "" || testCase.name == "parent query failure") {
				t.Fatal("父目录检查失败后仍执行 INSERT")
			}
		})
	}
}

func TestCreateUserDirectoryInputAndTransaction(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	script := &loginTransactionScript{}
	store := revokeSessionTestStore(t, actor, script)
	for _, candidate := range []*Store{store, {database: store.database, loginUserID: 7, authenticatedActor: &actor}} {
		if entry, err := candidate.CreateUserDirectory(context.Background(), 19, "资料", []byte("资料"), nil); entry != nil || err == nil || script.sequence() != "" {
			t.Fatal("普通 Store 被允许创建目录")
		}
	}
	err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
		cancelled, cancel := context.WithCancel(txContext)
		cancel()
		for _, testCase := range []struct {
			ctx      context.Context
			parentID uint64
			name     string
			key      []byte
			wantErr  error
		}{
			{parentID: 19, name: "资料", key: []byte("资料")},
			{ctx: txContext, name: "资料", key: []byte("资料")},
			{ctx: txContext, parentID: 19, key: []byte("资料"), wantErr: domain.ErrInvalidEntryName},
			{ctx: txContext, parentID: 19, name: "资料\xff", key: []byte("资料"), wantErr: domain.ErrInvalidEntryName},
			{ctx: txContext, parentID: 19, name: "资料", wantErr: domain.ErrInvalidEntryName},
			{ctx: txContext, parentID: 19, name: "资料", key: bytes.Repeat([]byte("a"), 769), wantErr: domain.ErrInvalidEntryName},
			{ctx: cancelled, parentID: 19, name: "资料", key: []byte("资料"), wantErr: context.Canceled},
		} {
			entry, err := txStore.CreateUserDirectory(testCase.ctx, testCase.parentID, testCase.name, testCase.key, nil)
			if entry != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) {
				t.Error("无效创建输入未拒绝", err)
			}
		}
		return nil
	})
	if err != nil || script.sequence() != "begin,lock,device,session,commit" {
		t.Fatal("无效输入执行了额外查询或写入", err, script.sequence())
	}
}

func TestCreateUserDirectoryCancellation(t *testing.T) {
	for _, phase := range []string{"during insert", "after insert", "after duplicate", "after read back"} {
		t.Run(phase, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			reads := 0
			script.entryQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				reads++
				if reads == 1 {
					return &fileEntryRows{values: []driver.Value{int64(19), int64(7), nil, "", "DIR", int64(0), int64(1), "ACTIVE", time.Now(), time.Now(), time.Now()}}, nil
				}
				cancel()
				return &fileEntryRows{}, nil
			}
			script.insert = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "during insert" {
					<-writeContext.Done()
					return nil, writeContext.Err()
				}
				if phase != "after read back" {
					cancel()
				}
				if phase == "after duplicate" {
					return nil, &mysql.MySQLError{Number: 1062, Message: "private cancelled duplicate"}
				}
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				entry, err := txStore.CreateUserDirectory(txContext, 19, "资料", []byte("资料"), nil)
				if entry != nil {
					t.Error("取消后返回了新建目录")
				}
				return err
			})
			wantErr := error(context.Canceled)
			if phase == "during insert" {
				wantErr = context.DeadlineExceeded
			}
			if !errors.Is(err, wantErr) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("取消原因丢失或仍提交", err)
			}
		})
	}
}
