package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCreateFileReuseEntryIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	actor := domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
	const parentID, originalID = 13, 31
	const originalName = "发布笔记.MD"
	const newName = "发布笔记 (1).MD"

	// 1. 为现有活动文件使用正式名称键，并读取其可见对象。
	originalKey, err := service.BuildEntryNameKey(originalName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name_key=? WHERE id=?", originalKey, originalID); err != nil {
		t.Fatal(err)
	}
	candidate, err := store.FindUserVisibleReusableObject(ctx, actor.UserID, [32]byte{}, domain.MaxSingleUploadSizeBytes)
	if err != nil || candidate == nil || candidate.ID != 29 {
		t.Fatal("未取到已有的秒传对象", candidate, err)
	}

	type databaseState struct {
		entries, objects, uploadTasks int
		used, reserved                uint64
	}
	readState := func(t *testing.T) databaseState {
		t.Helper()
		var state databaseState
		err := pool.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM file_entries),(SELECT COUNT(*) FROM stored_objects),
			(SELECT COUNT(*) FROM upload_tasks), used_storage, reserved_storage FROM users WHERE id=?`, actor.UserID).
			Scan(&state.entries, &state.objects, &state.uploadTasks, &state.used, &state.reserved)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := readState(t)

	// 2. 通过正式账号事务尝试写入，覆盖冲突、过期候选及外层回滚。
	rollback := errors.New("模拟后续写入失败")
	for _, scenario := range []struct {
		name      string
		wantError error
	}{
		{"active duplicate", domain.ErrEntryNameConflict},
		{"object size mismatch", domain.ErrInvalidStoredObjectData},
		{"device name changed", domain.ErrInvalidDeviceData},
		{"source no longer visible", domain.ErrReusableContentNotFound},
		{"outer rollback", rollback},
		{"commit with shared object", nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var saved *domain.FileEntry
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := service.ValidateFileReuseTarget(txContext, actor, parentID, txStore); err != nil {
					return err
				}
				source, err := txStore.FindCurrentUserDevice(txContext)
				if err != nil {
					return err
				}
				name := newName
				if scenario.wantError == domain.ErrEntryNameConflict {
					name = originalName
				}
				input, err := service.BuildFileReuseEntryInput(actor, parentID, name, candidate, source)
				if err != nil {
					return err
				}
				switch scenario.wantError {
				case domain.ErrInvalidStoredObjectData:
					input.SizeBytes++
				case domain.ErrInvalidDeviceData:
					if _, err := txStore.RenameUserDevice(txContext, actor.DeviceID, "已改名设备"); err != nil {
						return err
					}
				case domain.ErrReusableContentNotFound:
					if _, err := txStore.TrashUserFileEntry(txContext, originalID, 1); err != nil {
						return err
					}
				}
				saved, err = txStore.CreateFileReuseEntry(txContext, candidate, *input)
				if err == nil && scenario.wantError == rollback {
					return rollback
				}
				return err
			})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("秒传目录项事务结果不符", err)
			}
			if scenario.wantError != nil {
				if (saved != nil) != (scenario.wantError == rollback) || readState(t) != before {
					t.Fatal("失败没有完整回滚", saved)
				}
				return
			}

			// 3. 保存后的新条目与原条目共用对象，物理记录及配额账目保持原值。
			after := readState(t)
			expected := before
			expected.entries++
			if after != expected || saved == nil || saved.ID == originalID || saved.Revision != 1 ||
				saved.EntryType != "FILE" || saved.Status != "ACTIVE" || saved.Name != newName || saved.FileSize != candidate.FileSize {
				t.Fatalf("新逻辑文件或数据库状态不符，条目 %+v，状态 %+v", saved, after)
			}
			for _, entryID := range []uint64{originalID, saved.ID} {
				content, err := store.FindUserFileEntryContent(ctx, actor.UserID, entryID)
				if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != candidate.ID {
					t.Fatal("新旧目录项未引用同一物理对象", content, err)
				}
			}
			display, err := store.FindUserFileEntryDisplay(ctx, actor.UserID, saved.ID)
			if err != nil || display == nil || display.SourceDeviceID == nil || *display.SourceDeviceID != actor.DeviceID ||
				display.SourceDeviceName == nil || *display.SourceDeviceName != "second-device" ||
				display.ContentModifiedAt == nil || display.ContentModifiedAt.After(saved.CreatedAt) {
				t.Fatal("来源设备或发布时间快照不符", display, err)
			}
			object, err := store.FindUserStoredObject(ctx, actor.UserID, candidate.ID)
			if err != nil || !reflect.DeepEqual(object, candidate) {
				t.Fatal("目录项写入改变了已有物理对象", object, err)
			}
		})
	}
	t.Log("新目录项引用同一 READY 对象，活动同名冲突、候选失效和外层回滚均已验证")
}
