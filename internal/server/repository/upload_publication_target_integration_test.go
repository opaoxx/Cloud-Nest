package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestValidateUploadPublicationTargetIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
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
	// 1. 准备实际目标和临界层级，并用同一条连接完成复核。
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(15,7,13,'ancestor',CAST('ancestor' AS BINARY),'DIR'),(16,7,15,'target',CAST('target' AS BINARY),'DIR')`)
	parentID := uint64(13)
	const depthBase = 100
	for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
		entryID := uint64(depthBase + depth)
		name := fmt.Sprintf("depth-%d", depth)
		execute(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,7,?,?,?,'DIR')", entryID, parentID, name, []byte(name))
		parentID = entryID
	}
	execute(t, "UPDATE stored_objects SET status='VERIFYING' WHERE id=23")
	staged, err := store.FindUserStoredObject(ctx, 7, 23)
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name        string
		targetID    uint64
		hiddenState string
		move        bool
		depth       int
		wantError   error
	}{
		{name: "root", targetID: 13},
		{name: "nested target", targetID: 16, depth: 2},
		{name: "target is file", targetID: 31, wantError: domain.ErrUploadTargetUnavailable},
		{name: "trashed ancestor", targetID: 16, hiddenState: "TRASHED", wantError: domain.ErrUploadTargetUnavailable},
		{name: "purging ancestor", targetID: 16, hiddenState: "PURGING", wantError: domain.ErrUploadTargetUnavailable},
		{name: "purged ancestor", targetID: 16, hiddenState: "PURGED", wantError: domain.ErrUploadTargetUnavailable},
		{name: "file fits at depth limit", targetID: depthBase + domain.MaxFileEntryDepth - 1, depth: domain.MaxFileEntryDepth - 1},
		{name: "file would exceed depth limit", targetID: depthBase + domain.MaxFileEntryDepth, wantError: domain.ErrUploadTargetUnavailable},
		{name: "moved ancestor makes target too deep", targetID: 16, move: true, wantError: domain.ErrUploadTargetUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			execute(t, `UPDATE upload_tasks SET target_parent_id=?, status='VERIFYING', upload_mode='SINGLE',
				storage_upload_id=NULL, part_size=NULL, part_count=NULL, published_object_id=NULL, resolved_name=NULL,
				result_entry_id=NULL, completed_at=NULL, reserved_bytes=file_size, lease_owner='verify-worker', lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6)
				WHERE id=19`, scenario.targetID)
			task, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			digest := domain.UploadContentDigest{SizeBytes: task.FileSize, SHA256: *task.ExpectedSHA256}

			// 2. 用发布事务实际的任务及路径查询复核目标。
			var path *domain.FileEntryPath
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
				if scenario.hiddenState != "" {
					state := scenario.hiddenState
					result := txStore.database.WithContext(txContext).Exec(`UPDATE file_entries SET status=?, deleted_at=UTC_TIMESTAMP(6),
						purge_started_at=IF(?='PURGING',UTC_TIMESTAMP(6),NULL),
						purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=15`, state, state, state)
					if result.Error != nil || result.RowsAffected != 1 {
						t.Fatal("未设置预期的祖先状态", result.Error)
					}
				}
				if scenario.move {
					original, err := service.ValidateUploadPublicationTarget(txContext, current, txStore)
					if err != nil || original == nil || len(original.Entries) != 3 {
						t.Fatal("移动前的路径应通过复核", err)
					}
					result := txStore.database.WithContext(txContext).Exec("UPDATE file_entries SET parent_id=? WHERE id=15", depthBase+domain.MaxFileEntryDepth-2)
					if result.Error != nil || result.RowsAffected != 1 {
						t.Fatal("未移动测试祖先", result.Error)
					}
				}
				var pathError error
				path, pathError = service.ValidateUploadPublicationTarget(txContext, current, txStore)
				return pathError
			})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("真实发布目标错误不符", err)
			}
			if scenario.wantError != nil {
				if path != nil {
					t.Fatal("目标不可用仍返回路径", path)
				}
				return
			}
			if path == nil || len(path.Entries) != scenario.depth+1 || path.Entries[0].ID != 13 ||
				path.Entries[len(path.Entries)-1].ID != scenario.targetID {
				t.Fatal("有效目标路径不符", path)
			}
		})
	}
}
