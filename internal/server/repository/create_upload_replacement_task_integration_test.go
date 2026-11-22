package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateUserUploadReplacementTaskIntegration(t *testing.T) {
	// 1. 在独占库准备 v5 上传表、授权记录、目标文件和已有暂存对象。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if sessionStart < 0 || sessionEnd <= sessionStart {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd],
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES (17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES (33,7,13,'parent',X'706172656E74','DIR')",
		"UPDATE file_entries SET parent_id=33,revision=6 WHERE id=31",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES (32,8,14,'other.bin',CAST('other.bin' AS BINARY),'FILE','OTHER',24,0)",
		`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES
			(50,7,'MINIO','fixture-bucket','replacement-empty',0),(51,7,'MINIO','fixture-bucket','replacement-single',16777216),
			(52,7,'MINIO','fixture-bucket','replacement-multipart',16777217),(53,7,'MINIO','fixture-bucket','replacement-unused',1)`,
		"UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备替换任务夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	const entryID, revision = 31, 6
	const singleLimit = domain.MaxSingleUploadSizeBytes
	limits := domain.UploadPlanLimits{MaxFileSizeBytes: 2 * singleLimit, MaxObjectSizeBytes: 2 * singleLimit, MaxPartSizeBytes: singleLimit}
	digest := sha256.Sum256([]byte("replacement task fixture"))
	expiry := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
	checkTask := func(t *testing.T, task *domain.UploadTask, fixtureObjectID, size uint64, expectedDigest [sha256.Size]byte, plan domain.UploadPlan, status domain.UploadStatus) {
		t.Helper()
		if task == nil || task.ID == 0 || task.UserID != 7 || task.DeviceID != 11 || task.StagedObjectID != fixtureObjectID ||
			task.TargetParentID != 33 || task.RequestedName != "发布笔记.MD" || task.FileSize != size || task.ReservedBytes != size ||
			task.ExpectedSHA256 == nil || *task.ExpectedSHA256 != expectedDigest ||
			task.ReplacementEntryID == nil || *task.ReplacementEntryID != entryID ||
			task.ReplacementExpectedRevision == nil || *task.ReplacementExpectedRevision != revision ||
			task.ReplacementPublishedRevision != nil || task.ReplacementConflictingRevision != nil ||
			task.UploadMode != plan.Mode || task.Status != status || !task.ExpiresAt.Equal(expiry) ||
			task.ResultEntryID != nil || task.PublishedObjectID != nil || task.ResolvedName != nil {
			t.Fatalf("替换任务初始事实不符 %+v", task)
		}
		if plan.Mode == "MULTIPART" {
			if task.PartSize == nil || *task.PartSize != plan.PartSizeBytes || task.PartCount == nil || *task.PartCount != plan.PartCount {
				t.Fatalf("分片计划未保存 %+v", task)
			}
		} else if task.PartSize != nil || task.PartCount != nil {
			t.Fatalf("SINGLE 任务含分片计划 %+v", task)
		}
	}

	// 2. 在同一已认证事务内准备并创建，再从另一读操作核对提交结果。
	for _, fixture := range []struct {
		name           string
		objectID, size uint64
		status         domain.UploadStatus
		expectedDigest [sha256.Size]byte
	}{
		{"zero byte single", 50, 0, domain.UploadStatusUploading, sha256.Sum256(nil)},
		{"single boundary", 51, singleLimit, domain.UploadStatusUploading, digest},
		{"multipart boundary", 52, singleLimit + 1, domain.UploadStatusCreating, digest},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var task *domain.UploadTask
			var plan domain.UploadPlan
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				declaration := service.UploadReplacementDeclaration{
					EntryID: entryID, ExpectedRevision: revision, SizeBytes: fixture.size, ExpectedSHA256: fixture.expectedDigest,
				}
				prepared, err := service.PrepareUploadReplacement(txContext, actor, declaration, limits, txStore)
				if err != nil {
					return err
				}
				plan = prepared.Plan
				input := domain.CreateUploadInput{
					ParentID: prepared.ParentID, Name: prepared.Name, SizeBytes: prepared.Declaration.SizeBytes,
					ExpectedSHA256: &prepared.Declaration.ExpectedSHA256,
				}
				task, err = txStore.CreateUserUploadReplacementTask(txContext, fixture.objectID, input, plan, entryID, revision, expiry)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			checkTask(t, task, fixture.objectID, fixture.size, fixture.expectedDigest, plan, fixture.status)
			stored, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID)
			if err != nil || !reflect.DeepEqual(stored, task) {
				t.Fatalf("提交后任务重读不符 %+v，错误 %v", stored, err)
			}
		})
	}

	write := func(objectID, targetID, expectedRevision uint64, input domain.CreateUploadInput, plan domain.UploadPlan) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			_, err := txStore.CreateUserUploadReplacementTask(txContext, objectID, input, plan, targetID, expectedRevision, expiry)
			return err
		})
	}
	base := domain.CreateUploadInput{ParentID: 33, Name: "发布笔记.MD", SizeBytes: 1, ExpectedSHA256: &digest}
	singlePlan := domain.UploadPlan{Mode: "SINGLE"}
	for _, fixture := range []struct {
		name             string
		objectID, target uint64
		version          uint64
		change           func(*domain.CreateUploadInput, *domain.UploadPlan)
		want             error
	}{
		{"missing single digest", 53, entryID, revision, func(input *domain.CreateUploadInput, _ *domain.UploadPlan) { input.ExpectedSHA256 = nil }, domain.ErrInvalidUploadTaskData},
		{"unknown mode", 53, entryID, revision, func(_ *domain.CreateUploadInput, plan *domain.UploadPlan) { plan.Mode = "other" }, domain.ErrInvalidUploadTaskData},
		{"wrong multipart layout", 52, entryID, revision, func(input *domain.CreateUploadInput, plan *domain.UploadPlan) {
			input.SizeBytes = singleLimit + 1
			*plan = domain.UploadPlan{Mode: "MULTIPART", PartSizeBytes: singleLimit, PartCount: 2, LastPartSizeBytes: 2}
		}, domain.ErrInvalidUploadTaskData},
		{"zero target", 53, 0, revision, nil, domain.ErrInvalidUploadTaskData},
		{"zero revision", 53, entryID, 0, nil, domain.ErrInvalidUploadTaskData},
		{"version mismatch", 53, entryID, 5, nil, domain.ErrEntryRevisionConflict},
		{"directory as target", 53, 33, 1, nil, domain.ErrFileEntryNotFile},
		{"foreign file", 53, 32, 1, nil, domain.ErrFileEntryNotFound},
		{"wrong parent snapshot", 53, entryID, revision, func(input *domain.CreateUploadInput, _ *domain.UploadPlan) { input.ParentID = 13 }, domain.ErrInvalidUploadTaskData},
		{"wrong name snapshot", 53, entryID, revision, func(input *domain.CreateUploadInput, _ *domain.UploadPlan) { input.Name = "发布笔记.md" }, domain.ErrInvalidUploadTaskData},
		{"wrong staged size", 50, entryID, revision, nil, domain.ErrInvalidUploadTaskData},
		{"foreign staged object", 24, entryID, revision, nil, domain.ErrStoredObjectNotFound},
		{"duplicate staged reference", 50, entryID, revision, func(input *domain.CreateUploadInput, _ *domain.UploadPlan) { input.SizeBytes = 0 }, domain.ErrUploadTaskStagedObjectConflict},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input, plan := base, singlePlan
			if fixture.change != nil {
				fixture.change(&input, &plan)
			}
			if err := write(fixture.objectID, fixture.target, fixture.version, input, plan); !errors.Is(err, fixture.want) {
				t.Fatalf("预期拒绝 %v，错误 %v", fixture.want, err)
			}
		})
	}

	// 3. 注入仅影响测试库的回读差异，确认事务回滚了新任务。
	if _, err := pool.ExecContext(ctx, `CREATE TRIGGER fixture_replacement_binding BEFORE INSERT ON upload_tasks FOR EACH ROW
		SET NEW.replacement_expected_revision = IF(NEW.staged_object_id=53,NEW.replacement_expected_revision+1,NEW.replacement_expected_revision)`); err != nil {
		t.Fatal(err)
	}
	if err := write(53, entryID, revision, base, singlePlan); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatalf("回读应拒绝不一致的替换绑定，错误 %v", err)
	}
	var state string
	if err := pool.QueryRowContext(ctx, `SELECT CONCAT_WS('|',e.revision,e.object_id,u.used_storage,u.reserved_storage,u.change_seq,
			(SELECT COUNT(*) FROM upload_tasks),(SELECT COUNT(*) FROM upload_tasks WHERE staged_object_id=53),
			(SELECT COUNT(*) FROM stored_objects),(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
			FROM file_entries e JOIN users u ON u.id=e.user_id WHERE e.id=31`).Scan(&state); err != nil ||
		state != "6|29|16777216|512|0|4|0|7|0|0" {
		t.Fatalf("任务回滚或文件与账目边界不符 %q，错误 %v", state, err)
	}
}
