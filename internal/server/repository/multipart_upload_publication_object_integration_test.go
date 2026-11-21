package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestSelectMultipartUploadPublicationObjectIntegration(t *testing.T) {
	const fileSize = domain.MaxSingleUploadSizeBytes + 1
	const requestedName = "Report.PDF"
	const sourceName = "  已改名设备  "
	digest := domain.UploadContentDigest{SizeBytes: fileSize, SHA256: sha256.Sum256([]byte("multipart publication fixture"))}
	rollback := errors.New("fixture rollback after object selection")
	for _, fixture := range []struct {
		name, change string
		existingUser uint64
		version      *string
		wantID       uint64
		wantError    error
	}{
		{name: "promote original version", version: new("merged-version"), wantID: 23},
		{name: "reuse matching object", existingUser: 7, version: new("merged-version"), wantID: 90},
		{name: "ignore foreign match", existingUser: 8, version: new("merged-version"), wantID: 23},
		{name: "rollback promotion with optional version", wantID: 23, wantError: rollback},
		{name: "session changed after recheck", change: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "part plan changed after recheck", change: "UPDATE upload_tasks SET part_count=3 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "declared hash removed after recheck", change: "UPDATE upload_tasks SET expected_sha256=NULL WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease expired after recheck", change: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "object version changed after recheck", change: "UPDATE stored_objects SET storage_version_id='other-version' WHERE id=23", version: new("merged-version"), wantError: domain.ErrUploadTaskBusy},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, pool, store := prepareMultipartCompletionDatabase(t)
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			readObject := func(id uint64) *domain.StoredObject {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, 7, id)
				if err != nil {
					t.Fatal(err)
				}
				return object
			}

			// 1. 准备同摘要夹具和已合并的暂存对象，领取正式校验租约。
			if fixture.existingUser != 0 {
				execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at,storage_version_id,storage_etag)
					VALUES(90,?,'local_minio','fixture-bucket','existing-ready',?,?,'READY',UTC_TIMESTAMP(6),'existing-version','existing-etag')`,
					fixture.existingUser, fileSize, digest.SHA256[:])
			}
			if fixture.existingUser == 7 {
				execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
					VALUES(91,7,13,'Report.PDF',CAST('report.pdf' AS BINARY),'FILE','DOCUMENT',90,?)`, fileSize)
			}
			execute("UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='MULTIPART',storage_upload_id='Fixture+A/= ',
				requested_name=?,file_size=?,part_size=?,part_count=2,expected_sha256=?,reserved_bytes=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				lease_owner=NULL,lease_until=NULL,lease_version=3,next_attempt_at=NULL,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19`, requestedName, fileSize, fileSize-1, digest.SHA256[:], fileSize)
			execute("UPDATE stored_objects SET file_size=?,status='VERIFYING',storage_version_id=?,storage_etag='merged-etag' WHERE id=23", fileSize, fixture.version)
			candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 {
				t.Fatal("未读到分片校验候选", candidates, err)
			}
			claimed, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], "fixture-verification")
			if err != nil || claimed == nil {
				t.Fatal("未提交校验租约", err)
			}
			execute("UPDATE user_devices SET name=?,revoked_at=UTC_TIMESTAMP(6) WHERE user_id=7 AND id=11", sourceName)
			staged := readObject(23)
			var existing *domain.StoredObject
			if fixture.existingUser == 7 {
				existing = readObject(90)
			}
			if selected, err := store.SelectUploadPublicationObject(ctx, claimed, staged, digest); selected != nil || err == nil {
				t.Fatal("事务外选择未被拒绝", err)
			}

			// 2. 复核目标并选择对象，按事务读取的名称和设备生成输入。
			var selected *domain.StoredObject
			var entryInput *domain.CreatePublishedFileEntryInput
			err = store.WithUploadVerificationTransaction(ctx, claimed, staged, digest,
				func(txContext context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
					if fixture.change != "" {
						result := txStore.database.WithContext(txContext).Exec(fixture.change)
						if result.Error != nil || result.RowsAffected != 1 {
							t.Fatal("未模拟预期的条件变化", result.Error)
						}
					}
					if _, err := service.ValidateUploadPublicationTarget(txContext, current, txStore); err != nil {
						return err
					}
					var selectionError error
					selected, selectionError = txStore.SelectUploadPublicationObject(txContext, current, object, digest)
					if selectionError != nil {
						return selectionError
					}
					name, err := service.ResolveUploadPublicationName(txContext, current.UserID, current.TargetParentID, current.RequestedName, txStore.FindUserActiveFileEntryByName)
					if err != nil {
						return err
					}
					device, err := txStore.FindUploadSourceDevice(txContext, current)
					if err != nil || device == nil || device.RevokedAt == nil {
						t.Fatal("未读到已撤销的历史来源设备", err)
					}
					entryInput, err = service.BuildPublishedFileEntryInput(current, selected, name, device)
					if err != nil {
						return err
					}
					if fixture.wantError == rollback {
						return rollback
					}
					return nil
				})
			if !errors.Is(err, fixture.wantError) {
				t.Fatal("选择结果不符", fixture.wantError, err)
			}
			if fixture.wantID != 0 {
				if selected == nil || selected.ID != fixture.wantID || selected.OwnerUserID != 7 || selected.Status != domain.StoredObjectStatusReady ||
					selected.VerifiedSHA256 == nil || *selected.VerifiedSHA256 != digest.SHA256 || selected.DedupHash == nil || *selected.DedupHash != digest.SHA256 {
					t.Fatal("未选择同账号的可引用对象", selected)
				}
				if fixture.wantID == staged.ID && (!sameStoredObjectLocation(selected, staged) || !sameUploadStorageText(selected.StorageETag, staged.StorageETag)) {
					t.Fatal("推进时改动了合并保存的定位")
				}
				wantName, wantNameKey := requestedName, "report.pdf"
				if fixture.existingUser == 7 {
					wantName, wantNameKey = "Report (1).PDF", "report (1).pdf"
				}
				if entryInput == nil || entryInput.ParentID != claimed.TargetParentID || entryInput.ObjectID != selected.ID ||
					entryInput.Name != wantName || string(entryInput.NameKey) != wantNameKey || entryInput.Category != "DOCUMENT" ||
					entryInput.Extension == nil || *entryInput.Extension != "pdf" || entryInput.SizeBytes != fileSize ||
					entryInput.SourceDeviceID != claimed.DeviceID || entryInput.SourceDeviceName != sourceName {
					t.Fatal("发布事务生成的文件条目参数不符", entryInput)
				}
				t.Logf("目录 %d，文件名 %s，选定对象 %d，%d 字节，来源设备 %d", entryInput.ParentID, entryInput.Name, entryInput.ObjectID, entryInput.SizeBytes, entryInput.SourceDeviceID)
			} else if selected != nil || entryInput != nil {
				t.Fatal("条件不成立仍生成了对象或参数")
			}

			// 3. 核对最终提交或回滚，任务、文件数和配额仍符合原夹具。
			wantStaged := staged
			if fixture.wantID == staged.ID && fixture.wantError == nil {
				wantStaged = selected
			}
			if saved := readObject(23); !reflect.DeepEqual(saved, wantStaged) {
				t.Fatal("暂存对象的提交或回滚不符", saved)
			}
			if existing != nil && (!reflect.DeepEqual(readObject(90), existing) || !reflect.DeepEqual(selected, existing)) {
				t.Fatal("复用改变了原 READY 对象")
			}
			if task, err := store.FindUserUploadTask(ctx, 7, 19); err != nil || !reflect.DeepEqual(task, claimed) {
				t.Fatal("对象选择改动了任务记录", err)
			}
			var reserved, used, files uint64
			wantFiles := uint64(1)
			if existing != nil {
				wantFiles++
			}
			if err := pool.QueryRowContext(ctx, `SELECT reserved_storage,used_storage,
				(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE') FROM users WHERE id=7`).Scan(&reserved, &used, &files); err != nil ||
				reserved != fileSize || used != 0 || files != wantFiles {
				t.Fatal("对象选择改变了文件条目或配额", reserved, used, files, err)
			}
		})
	}
}
