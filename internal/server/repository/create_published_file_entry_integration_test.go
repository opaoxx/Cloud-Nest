package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCreatePublishedFileEntryIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(15,7,13,'归档',CAST('归档' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))")
	// 同名测试使用与正式输入相同的归一化键。
	const duplicateName = "发布笔记.MD"
	duplicateKey, err := service.BuildEntryNameKey(duplicateName)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, "UPDATE file_entries SET name_key=? WHERE user_id=7 AND id=31", duplicateKey)
	countEntries := func(t *testing.T) int {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	rollbackFixture := errors.New("fixture rolls back after creating file")

	for _, scenario := range []struct {
		name      string
		parent    uint64
		changeSQL string
		mimeType  *string
		duplicate bool
		zeroBytes bool
		wantError error
	}{
		{name: "publish with reused object", parent: 13, mimeType: new("image/png")},
		{name: "reject parameterized MIME", parent: 13, mimeType: new("text/plain; charset=utf-8"), wantError: domain.ErrInvalidFileEntryData},
		{name: "duplicate entry name", parent: 13, duplicate: true, wantError: domain.ErrEntryNameConflict},
		{name: "trashed destination", parent: 15, wantError: domain.ErrFileEntryNotFound},
		{name: "file destination", parent: 31, wantError: domain.ErrFileEntryNotDirectory},
		{name: "changed lease version", parent: 13, changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease expires after initial verification", parent: 13, changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "task expires after initial verification", parent: 13, changeSQL: "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskNotReady},
		{name: "selected object stops being ready", parent: 13, changeSQL: "UPDATE stored_objects SET status='DELETING' WHERE id=29", wantError: domain.ErrInvalidStoredObjectData},
		{name: "source name changes", parent: 13, changeSQL: "UPDATE user_devices SET name='renamed fixture' WHERE id=11", wantError: domain.ErrInvalidDeviceData},
		{name: "rollback new object and file", parent: 13, mimeType: new("image/jpeg"), wantError: rollbackFixture},
		{name: "publish zero-byte file", parent: 13, zeroBytes: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 使任务和暂存对象进入校验阶段，保留可复用的 READY 夹具。
			digest := domain.UploadContentDigest{SizeBytes: domain.MaxSingleUploadSizeBytes}
			if scenario.wantError == rollbackFixture {
				digest.SHA256[0] = 0xff
			}
			if scenario.zeroBytes {
				digest = domain.UploadContentDigest{SHA256: sha256.Sum256(nil)}
			}
			execute(t, `UPDATE stored_objects SET status='VERIFYING',file_size=?,verified_sha256=NULL,verified_at=NULL WHERE id=23`, digest.SizeBytes)
			execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				target_parent_id=?,file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6)
				WHERE id=19`, scenario.parent, digest.SizeBytes, digest.SizeBytes, digest.SHA256[:])
			task, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			before := countEntries(t)

			// 2. 复用真实写回复核、对象选择和输入构建。
			var saved *domain.FileEntry
			var input *domain.CreatePublishedFileEntryInput
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, currentTask *domain.UploadTask, currentObject *domain.StoredObject) error {
				selected, err := txStore.SelectUploadPublicationObject(txContext, currentTask, currentObject, digest)
				if err != nil {
					return err
				}
				source, err := txStore.FindUploadSourceDevice(txContext, currentTask)
				if err != nil {
					return err
				}
				resolvedName := scenario.name + ".PDF"
				if scenario.duplicate {
					resolvedName = duplicateName
				}
				if scenario.zeroBytes {
					resolvedName = ".env"
				}
				input, err = service.BuildPublishedFileEntryInput(currentTask, selected, resolvedName, source)
				if err != nil {
					return err
				}
				input.MIMEType = scenario.mimeType
				if scenario.changeSQL != "" {
					changed := txStore.database.WithContext(txContext).Exec(scenario.changeSQL)
					if changed.Error != nil || changed.RowsAffected != 1 {
						t.Fatal("条件变化夹具设置失败", changed.Error)
					}
				}
				saved, err = txStore.CreatePublishedFileEntry(txContext, currentTask, *input)
				if err == nil && scenario.wantError == rollbackFixture {
					return rollbackFixture
				}
				return err
			})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("事务结果不符", err)
			}

			// 3. 核对提交后的文件及物理引用，或确认外层清除了写入。
			if scenario.wantError != nil {
				if countEntries(t) != before || (scenario.wantError != rollbackFixture && saved != nil) {
					t.Fatal("失败或回滚留下了文件目录项", saved)
				}
				after, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil || !reflect.DeepEqual(after, staged) {
					t.Fatal("失败或回滚改变了暂存对象", after, err)
				}
				return
			}
			if saved == nil || input == nil || countEntries(t) != before+1 {
				t.Fatal("发布目录项未成功提交", saved)
			}
			actual, err := store.FindUserActiveFileEntryByName(ctx, 7, input.ParentID, input.NameKey)
			if err != nil || !reflect.DeepEqual(actual, saved) || saved.Revision != 1 || saved.FileSize != digest.SizeBytes {
				t.Fatal("新文件基础记录读回不一致", actual, err)
			}
			display, err := store.FindUserFileEntryDisplay(ctx, 7, saved.ID)
			if err != nil || display == nil || display.Category == nil || *display.Category != input.Category ||
				!reflect.DeepEqual(display.Extension, input.Extension) || !reflect.DeepEqual(display.MIMEType, scenario.mimeType) ||
				display.SourceDeviceID == nil || *display.SourceDeviceID != task.DeviceID ||
				display.SourceDeviceName == nil || *display.SourceDeviceName != "fixture" ||
				display.ContentModifiedAt == nil || display.ContentModifiedAt.After(saved.CreatedAt) {
				t.Fatal("文件展示字段或来源设备读回不一致", display, err)
			}
			var objectID uint64
			err = pool.QueryRowContext(ctx, "SELECT object_id FROM file_entries WHERE user_id=7 AND id=?", saved.ID).Scan(&objectID)
			if err != nil || objectID != input.ObjectID {
				t.Fatal("发布文件引用了错误的对象", objectID, err)
			}
		})
	}
}
