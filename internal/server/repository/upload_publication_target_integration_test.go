package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestValidateUploadPublicationTargetIntegration(t *testing.T) {
	for _, mode := range []string{"SINGLE", "MULTIPART"} {
		t.Run(mode, func(t *testing.T) { verifyUploadPublicationTargetPath(t, mode) })
	}
}

func verifyUploadPublicationTargetPath(t *testing.T, mode string) {
	t.Helper()
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	// 1. 准备合成目标和临界层级，选择对应的候选与租约入口。
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
	fileSize := domain.MaxSingleUploadSizeBytes
	var session, partSize, partCount any
	scan := store.ListUploadVerificationCandidates
	claim := store.ClaimUploadVerificationLease
	if mode == "MULTIPART" {
		fileSize++
		session, partSize, partCount = "fixture-session", domain.MaxSingleUploadSizeBytes, 2
		scan = store.ListMultipartUploadVerificationCandidates
		claim = store.ClaimMultipartUploadVerificationLease
	}
	execute(t, "UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
	execute(t, "UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23", fileSize)
	staged, err := store.FindUserStoredObject(ctx, 7, 23)
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name        string
		targetID    uint64
		hiddenID    uint64
		hiddenState string
		moveTo      uint64
		depth       int
		wantError   error
	}{
		{name: "root", targetID: 13},
		{name: "nested target", targetID: 16, depth: 2},
		{name: "target is file", targetID: 31, wantError: domain.ErrUploadTargetUnavailable},
		{name: "trashed target", targetID: 16, hiddenID: 16, hiddenState: "TRASHED", wantError: domain.ErrUploadTargetUnavailable},
		{name: "trashed ancestor", targetID: 16, hiddenID: 15, hiddenState: "TRASHED", wantError: domain.ErrUploadTargetUnavailable},
		{name: "purging ancestor", targetID: 16, hiddenID: 15, hiddenState: "PURGING", wantError: domain.ErrUploadTargetUnavailable},
		{name: "purged ancestor", targetID: 16, hiddenID: 15, hiddenState: "PURGED", wantError: domain.ErrUploadTargetUnavailable},
		{name: "file fits at depth limit", targetID: depthBase + domain.MaxFileEntryDepth - 1, depth: domain.MaxFileEntryDepth - 1},
		{name: "file would exceed depth limit", targetID: depthBase + domain.MaxFileEntryDepth, wantError: domain.ErrUploadTargetUnavailable},
		{name: "moved ancestor stays visible", targetID: 16, moveTo: depthBase + 1, depth: 3},
		{name: "moved ancestor makes target too deep", targetID: 16, moveTo: depthBase + domain.MaxFileEntryDepth - 2, wantError: domain.ErrUploadTargetUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			execute(t, `UPDATE upload_tasks SET target_parent_id=?, status='VERIFYING', upload_mode=?,
				storage_upload_id=?, part_size=?, part_count=?, published_object_id=NULL, resolved_name=NULL,
				result_entry_id=NULL, completed_at=NULL, file_size=?, reserved_bytes=?,
				lease_owner=NULL, lease_version=3, lease_until=NULL, next_attempt_at=NULL,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`,
				scenario.targetID, mode, session, partSize, partCount, fileSize, fileSize)
			candidates, err := scan(ctx, 1)
			if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
				t.Fatal("未找到本轮待校验任务", candidates, err)
			}
			task, err := claim(ctx, candidates[0], "verify-worker")
			if err != nil || task == nil || task.LeaseVersion != 4 || task.ExpectedSHA256 == nil {
				t.Fatal("未取得有效校验租约", err)
			}
			digest := domain.UploadContentDigest{SizeBytes: task.FileSize, SHA256: *task.ExpectedSHA256}

			// 2. 在校验租约取得后提交目录变化，再进入独立的发布复核事务。
			if scenario.hiddenID != 0 {
				state := scenario.hiddenState
				execute(t, `UPDATE file_entries SET status=?, deleted_at=UTC_TIMESTAMP(6),
					purge_started_at=IF(?='PURGING',UTC_TIMESTAMP(6),NULL),
					purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=?`, state, state, state, scenario.hiddenID)
				t.Cleanup(func() {
					execute(t, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=?", scenario.hiddenID)
				})
			}
			if scenario.moveTo != 0 {
				execute(t, "UPDATE file_entries SET parent_id=? WHERE id=15", scenario.moveTo)
				t.Cleanup(func() { execute(t, "UPDATE file_entries SET parent_id=13 WHERE id=15") })
			}
			var path *domain.FileEntryPath
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
				var pathError error
				path, pathError = service.ValidateUploadPublicationTarget(txContext, current, txStore)
				return pathError
			})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("真实发布目标错误不符", err)
			}
			// 3. 检查路径结果及只读复核前后的任务和对象。
			if savedTask, err := store.FindUserUploadTask(ctx, 7, 19); err != nil || !reflect.DeepEqual(savedTask, task) {
				t.Fatal("目录复核改变了任务或租约", err)
			}
			if savedObject, err := store.FindUserStoredObject(ctx, 7, 23); err != nil || !reflect.DeepEqual(savedObject, staged) {
				t.Fatal("目录复核改变了暂存对象", err)
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
			if scenario.moveTo != 0 && path.Entries[1].ID != scenario.moveTo {
				t.Fatal("发布复核没有读到移动后的祖先")
			}
		})
	}
	var reserved, used, files uint64
	if err := pool.QueryRowContext(ctx, `SELECT reserved_storage,used_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE') FROM users WHERE id=7`).Scan(&reserved, &used, &files); err != nil ||
		reserved != fileSize || used != 0 || files != 1 {
		t.Fatal("目录复核改变了配额或文件数", reserved, used, files, err)
	}
	t.Logf("%s 目标复核保留原任务、对象及 %d 字节预占", mode, reserved)
}
