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

type createUploadReplacementServiceStore struct{ *createUploadServiceStore }

func (store *createUploadReplacementServiceStore) CreateUserUploadReplacementTask(ctx context.Context, objectID uint64,
	input domain.CreateUploadInput, plan domain.UploadPlan, entryID, revision uint64, expiresAt time.Time,
) (*domain.UploadTask, error) {
	task, err := store.Store.CreateUserUploadReplacementTask(ctx, objectID, input, plan, entryID, revision, expiresAt)
	if err != nil {
		return nil, err
	}
	if store.failStage == "binding" {
		*task.ReplacementExpectedRevision = revision + 1
	}
	return task, store.finishCreateStep("task")
}

func TestCreateUploadReplacementInTransactionIntegration(t *testing.T) {
	// 1. 准备已存在的合成文件、活动会话和一笔无关的 512 B 预占。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if sessionStart < 0 || sessionEnd <= sessionStart {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd],
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES (17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"UPDATE file_entries SET revision=6 WHERE id=31",
		"UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备核心事务夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	const singleLimit = domain.MaxSingleUploadSizeBytes
	limits := domain.UploadPlanLimits{MaxFileSizeBytes: 2 * singleLimit, MaxObjectSizeBytes: 2 * singleLimit, MaxPartSizeBytes: singleLimit}
	declaration := service.UploadReplacementDeclaration{
		EntryID: 31, ExpectedRevision: 6, SizeBytes: 40, ExpectedSHA256: sha256.Sum256([]byte("isolated replacement fixture")),
	}
	failure := errors.New("fixture replacement write aborted")
	create := func(input service.UploadReplacementDeclaration, failStage string) (*domain.UploadTask, error) {
		var task *domain.UploadTask
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			transaction := &createUploadReplacementServiceStore{&createUploadServiceStore{Store: txStore, failStage: failStage, failure: failure}}
			var err error
			task, err = service.CreateUploadReplacementInTransaction(txContext, actor, input, limits, "MINIO", "fixture-bucket", transaction)
			if err == nil && failStage == "after task callback" {
				return failure
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		return task, nil
	}
	type state struct {
		used, reserved, reservedByTasks, objects, tasks, version, currentObject, currentSize, history, changes, sequence uint64
	}
	readState := func(t *testing.T) state {
		t.Helper()
		var result state
		err := pool.QueryRowContext(ctx, `SELECT u.used_storage,u.reserved_storage,
			(SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=u.id AND replacement_entry_id=31),
			(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=u.id),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=u.id),
			e.revision,e.object_id,e.file_size,(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events),u.change_seq
			FROM users u JOIN file_entries e ON e.user_id=u.id WHERE u.id=7 AND e.id=31`).
			Scan(&result.used, &result.reserved, &result.reservedByTasks, &result.objects, &result.tasks,
				&result.version, &result.currentObject, &result.currentSize, &result.history, &result.changes, &result.sequence)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	checkCreation := func(t *testing.T, input service.UploadReplacementDeclaration, status domain.UploadStatus) {
		t.Helper()
		before := readState(t)
		start := time.Now().UTC().Truncate(time.Microsecond)
		task, err := create(input, "")
		if err != nil || task == nil || task.Status != status || task.ReservedBytes != input.SizeBytes ||
			task.ExpectedSHA256 == nil || *task.ExpectedSHA256 != input.ExpectedSHA256 ||
			task.ReplacementEntryID == nil || *task.ReplacementEntryID != input.EntryID ||
			task.ReplacementExpectedRevision == nil || *task.ReplacementExpectedRevision != input.ExpectedRevision ||
			task.ExpiresAt.Before(start.Add(24*time.Hour)) || task.ExpiresAt.After(time.Now().Add(24*time.Hour)) {
			t.Fatalf("核心事务的已提交任务不符 %+v，错误 %v", task, err)
		}
		fresh, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID)
		if err != nil || !reflect.DeepEqual(task, fresh) {
			t.Fatalf("已提交任务重读不符 %+v，错误 %v", fresh, err)
		}
		object, err := store.FindUserStoredObject(ctx, actor.UserID, task.StagedObjectID)
		if err != nil || object == nil || object.Status != domain.StoredObjectStatusStaging || object.FileSize != input.SizeBytes ||
			object.Bucket != "fixture-bucket" || !strings.HasPrefix(object.ObjectKey, "users/7/objects/") {
			t.Fatalf("已提交暂存对象不符 %+v，错误 %v", object, err)
		}
		want := before
		want.reserved += input.SizeBytes
		want.reservedByTasks += input.SizeBytes
		want.tasks++
		want.objects++
		if after := readState(t); after != want || after.reserved != after.reservedByTasks+512 {
			t.Fatalf("预占、暂存对象与任务的提交不一致，得到 %+v，期望 %+v", after, want)
		}
	}

	// 2. 核对三种尺寸的完整提交和原内容保留。
	for _, fixture := range []struct {
		name   string
		size   uint64
		status domain.UploadStatus
	}{
		{"zero bytes", 0, domain.UploadStatusUploading},
		{"single boundary", singleLimit, domain.UploadStatusUploading},
		{"multipart boundary", singleLimit + 1, domain.UploadStatusCreating},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := declaration
			input.SizeBytes = fixture.size
			if input.SizeBytes == 0 {
				input.ExpectedSHA256 = sha256.Sum256(nil)
			}
			checkCreation(t, input, fixture.status)
		})
	}

	// 3. 在每项写入完成后注入失败，确认外层回滚并可用相同声明再创建。
	for _, stage := range []string{"reserve", "object", "task", "binding", "after task callback"} {
		t.Run("rollback "+stage, func(t *testing.T) {
			before := readState(t)
			want := failure
			if stage == "binding" {
				want = domain.ErrInvalidUploadTaskData
			}
			task, err := create(declaration, stage)
			if task != nil || !errors.Is(err, want) || readState(t) != before {
				t.Fatalf("阶段 %s 回滚不完整，任务 %+v，错误 %v", stage, task, err)
			}
		})
	}
	t.Run("revision before quota and exact quota", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=used_storage+reserved_storage+? WHERE id=7", declaration.SizeBytes); err != nil {
			t.Fatal(err)
		}
		before := readState(t)
		tooLarge := declaration
		tooLarge.SizeBytes++
		if task, err := create(tooLarge, ""); task != nil || !errors.Is(err, domain.ErrQuotaExceeded) || readState(t) != before {
			t.Fatalf("额度不足应拒绝全部写入，任务 %+v，错误 %v", task, err)
		}
		tooLarge.ExpectedRevision--
		if task, err := create(tooLarge, ""); task != nil || !errors.Is(err, domain.ErrEntryRevisionConflict) || readState(t) != before {
			t.Fatalf("准备版本冲突应优先，任务 %+v，错误 %v", task, err)
		}
		checkCreation(t, declaration, domain.UploadStatusUploading)
	})
}
