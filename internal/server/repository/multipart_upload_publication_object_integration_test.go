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
	digest := domain.UploadContentDigest{SizeBytes: fileSize, SHA256: sha256.Sum256([]byte("multipart publication fixture")), DetectedMIMEType: "application/pdf"}
	rollback := errors.New("fixture rollback after file creation")
	for _, fixture := range []struct {
		name, change       string
		changeBeforeInsert string
		existingUser       uint64
		version            *string
		wantID             uint64
		wantError          error
	}{
		{name: "promote original version", version: new("merged-version"), wantID: 23},
		{name: "reuse matching object", existingUser: 7, version: new("merged-version"), wantID: 90},
		{name: "ignore foreign match", existingUser: 8, version: new("merged-version"), wantID: 23},
		{name: "rollback promotion and file with optional version", wantID: 23, wantError: rollback},
		{name: "session changed after recheck", change: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "part plan changed after recheck", change: "UPDATE upload_tasks SET part_count=3 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "declared hash removed after recheck", change: "UPDATE upload_tasks SET expected_sha256=NULL WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease expired after recheck", change: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "object version changed after recheck", change: "UPDATE stored_objects SET storage_version_id='other-version' WHERE id=23", version: new("merged-version"), wantError: domain.ErrUploadTaskBusy},
		{name: "session changed before insert", changeBeforeInsert: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", wantID: 23, wantError: domain.ErrUploadTaskBusy},
		{name: "lease expired before insert", changeBeforeInsert: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantID: 23, wantError: domain.ErrUploadTaskBusy},
		{name: "target trashed before insert", changeBeforeInsert: "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=15", wantID: 23, wantError: domain.ErrFileEntryNotFound},
		{name: "selected object no longer ready", changeBeforeInsert: "UPDATE stored_objects SET status='DELETING' WHERE id=23", wantID: 23, wantError: domain.ErrInvalidStoredObjectData},
		{name: "selected object checksum changed", changeBeforeInsert: "UPDATE stored_objects SET verified_sha256=UNHEX(REPEAT('ff',32)) WHERE id=23", wantID: 23, wantError: domain.ErrUploadChecksumMismatch},
		{name: "source changed before insert", changeBeforeInsert: "UPDATE user_devices SET name='Another name' WHERE id=11", wantID: 23, wantError: domain.ErrInvalidDeviceData},
		{name: "name conflict at insert", changeBeforeInsert: "INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(7,15,'Report.PDF',CAST('report.pdf' AS BINARY),'DIR')", wantID: 23, wantError: domain.ErrEntryNameConflict},
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
			execute("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(15,7,13,'target',CAST('target' AS BINARY),'DIR')")
			if fixture.existingUser != 0 {
				execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at,storage_version_id,storage_etag)
					VALUES(90,?,'local_minio','fixture-bucket','existing-ready',?,?,'READY',UTC_TIMESTAMP(6),'existing-version','existing-etag')`,
					fixture.existingUser, fileSize, digest.SHA256[:])
			}
			if fixture.existingUser == 7 {
				execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
					VALUES(91,7,15,'Report.PDF',CAST('report.pdf' AS BINARY),'FILE','DOCUMENT',90,?)`, fileSize)
			}
			execute("UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='MULTIPART',storage_upload_id='Fixture+A/= ',
				target_parent_id=15,requested_name=?,file_size=?,part_size=?,part_count=2,expected_sha256=?,reserved_bytes=?,
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

			// 2. 复核目标及对象，使用发布事务生成的输入创建文件。
			var selected *domain.StoredObject
			var entryInput *domain.CreatePublishedFileEntryInput
			var entry *domain.FileEntry
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
					entryInput.MIMEType = &digest.DetectedMIMEType
					if fixture.changeBeforeInsert != "" {
						result := txStore.database.WithContext(txContext).Exec(fixture.changeBeforeInsert)
						if result.Error != nil || result.RowsAffected != 1 {
							t.Fatal("未模拟预期的写入前变化", result.Error)
						}
					}
					entry, err = txStore.CreatePublishedFileEntry(txContext, current, *entryInput)
					if err != nil {
						return err
					}
					if fixture.wantError == rollback {
						return rollback
					}
					return nil
				})
			if !errors.Is(err, fixture.wantError) {
				t.Fatal("文件创建事务结果不符", fixture.wantError, err)
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
			} else if selected != nil || entryInput != nil {
				t.Fatal("条件不成立仍生成了对象或参数")
			}

			// 3. 核对文件及展示快照的提交结果，以及对象和条目的一起回滚。
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
			if fixture.wantError == nil || fixture.wantError == rollback {
				if entry == nil || entry.ID == 0 || entry.ParentID == nil || *entry.ParentID != entryInput.ParentID ||
					entry.Name != entryInput.Name || entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.Revision != 1 ||
					entry.FileSize != fileSize || !entry.CreatedAt.Before(*claimed.LeaseUntil) {
					t.Fatal("新文件基础记录不符", entry)
				}
				persisted, err := store.FindUserActiveFileEntryByName(ctx, 7, entryInput.ParentID, entryInput.NameKey)
				if fixture.wantError == rollback {
					if persisted != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
						t.Fatal("外层回滚后仍能查到新文件", persisted, err)
					}
				} else {
					wantFiles++
					if err != nil || !reflect.DeepEqual(persisted, entry) {
						t.Fatal("提交后的目录项与事务返回不一致", persisted, err)
					}
					display, err := store.FindUserFileEntryDisplay(ctx, 7, entry.ID)
					if err != nil || display == nil || !reflect.DeepEqual(display.Category, &entryInput.Category) ||
						!reflect.DeepEqual(display.Extension, entryInput.Extension) || !reflect.DeepEqual(display.MIMEType, entryInput.MIMEType) ||
						!reflect.DeepEqual(display.SourceDeviceID, &entryInput.SourceDeviceID) || !reflect.DeepEqual(display.SourceDeviceName, &entryInput.SourceDeviceName) ||
						display.ContentModifiedAt == nil || display.ContentModifiedAt.After(entry.CreatedAt) {
						t.Fatal("文件展示字段或来源快照不符", display, err)
					}
					var publishedObjectID uint64
					if err := pool.QueryRowContext(ctx, "SELECT object_id FROM file_entries WHERE user_id=7 AND id=?", entry.ID).Scan(&publishedObjectID); err != nil || publishedObjectID != fixture.wantID {
						t.Fatal("新文件引用了错误的对象", publishedObjectID, err)
					}
					t.Logf("已提交文件 %d，目录 %d，名称 %s，引用对象 %d，%d 字节", entry.ID, entryInput.ParentID, entry.Name, publishedObjectID, entry.FileSize)
				}
			} else if entry != nil {
				t.Fatal("前置条件未通过仍返回文件", entry)
			}
			if err := pool.QueryRowContext(ctx, `SELECT reserved_storage,used_storage,
				(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE') FROM users WHERE id=7`).Scan(&reserved, &used, &files); err != nil ||
				reserved != fileSize || used != 0 || files != wantFiles {
				t.Fatal("文件数量或配额不符", reserved, used, files, err)
			}
		})
	}
}
