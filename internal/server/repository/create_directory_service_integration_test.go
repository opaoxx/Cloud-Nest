package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

// createDirectoryResultStore 在真实插入后注入结果异常或取消，验证服务与事务的衔接。
type createDirectoryResultStore struct {
	*Store
	afterCreate func(*domain.FileEntry)
}

func (store *createDirectoryResultStore) CreateUserDirectory(ctx context.Context, parentID uint64, name string, nameKey []byte, colorKey *string) (*domain.FileEntry, error) {
	entry, err := store.Store.CreateUserDirectory(ctx, parentID, name, nameKey, colorKey)
	if err == nil && store.afterCreate != nil {
		store.afterCreate(entry)
	}
	return entry, err
}

func TestCreateDirectoryServiceIntegration(t *testing.T) {
	// 1. 沿用正式目录项独占库，补充认证会话表并建立可见路径和隐藏祖先。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表边界错误")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','目录服务用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'父目录',CAST('父目录' AS BINARY),'DIR'),(14,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR'),(15,1,14,'普通父目录',CAST('普通父目录' AS BINARY),'DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','service-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,11,'file.txt',CAST('file.txt' AS BINARY),'FILE','DOCUMENT',41,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	newActor := func(userID uint64) domain.Actor {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		if err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "目录服务设备", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		token, err := signer.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("真实 JWT 身份验证失败", err)
		}
		return *actor
	}
	actor, otherActor := newActor(1), newActor(3)
	creator := service.NewCreateDirectoryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.CreateDirectoryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	countEntries := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	missing := func(t *testing.T, name string) {
		t.Helper()
		key, err := service.BuildEntryNameKey(name)
		if err != nil {
			t.Fatal(err)
		}
		if entry, err := store.FindUserActiveFileEntryByName(ctx, 1, 12, key); entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("回滚后仍存在新目录", err)
		}
	}

	// 2. 单连接复用整个路径和写入流程，真实名称及颜色保持既定语义。
	t.Run("single transaction defaults and unicode", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		color := "green"
		entry, err := creator.Create(ctx, actor, 12, "  Cafe\u0301  ", &color)
		if err != nil || entry == nil || entry.UserID != 1 || entry.Name != "Cafe\u0301" || entry.ParentID == nil || *entry.ParentID != 12 || entry.EntryType != "DIR" || entry.FileSize != 0 || entry.Revision != 1 || entry.Status != "ACTIVE" {
			t.Fatal("目录服务结果错误", err)
		}
		if entry.CreatedAt.IsZero() || entry.CreatedAt.Location() != time.UTC || !entry.CreatedAt.Equal(entry.ModifiedAt) || !entry.CreatedAt.Equal(entry.UpdatedAt) || entry.CreatedAt.Nanosecond()%1000 != 0 {
			t.Fatal("数据库默认时间错误")
		}
		var key []byte
		if err := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE id=?", entry.ID).Scan(&key); err != nil || string(key) != "café" {
			t.Fatal("服务未生成规范化名称键", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, entry.ID)
		if err != nil || display.ColorKey == nil || *display.ColorKey != "green" {
			t.Fatal("服务未写入颜色", err)
		}
		*entry.ParentID = 999
		if actual, err := store.FindUserFileEntry(ctx, 1, entry.ID); err != nil || *actual.ParentID != 12 {
			t.Fatal("本地修改影响数据库记录", err)
		}
	})
	t.Run("conflict scopes and optional colors", func(t *testing.T) {
		if entry, err := creator.Create(ctx, actor, 12, "CAFÉ", nil); entry != nil || !errors.Is(err, domain.ErrEntryNameConflict) {
			t.Fatal("规范等价重名未拒绝", err)
		}
		if _, err := creator.Create(ctx, actor, 11, "Café", nil); err != nil {
			t.Fatal("根目录创建失败", err)
		}
		empty := ""
		other, err := creator.Create(ctx, otherActor, 33, "Café", &empty)
		if err != nil || other.UserID != 3 {
			t.Fatal("跨用户独立名称失败", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 3, other.ID)
		if err != nil || display.ColorKey == nil || *display.ColorKey != "" {
			t.Fatal("显式空颜色未保留", err)
		}
	})
	t.Run("parent scope type and all hidden ancestor states", func(t *testing.T) {
		before := countEntries()
		for _, testCase := range []struct {
			parentID uint64
			want     error
		}{{999, domain.ErrFileEntryNotFound}, {33, domain.ErrFileEntryNotFound}, {13, domain.ErrFileEntryNotDirectory}} {
			if entry, err := creator.Create(ctx, actor, testCase.parentID, "不可创建", nil); entry != nil || !errors.Is(err, testCase.want) {
				t.Fatal("父目录范围或类型检查失败", err)
			}
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=14"); err != nil {
				t.Error(err)
			}
		}()
		for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='PURGING',UTC_TIMESTAMP(6),NULL),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=14", state, state, state); err != nil {
				t.Fatal(err)
			}
			for _, parentID := range []uint64{14, 15} {
				if entry, err := creator.Create(ctx, actor, parentID, "隐藏路径", nil); entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
					t.Fatal("非活动父目录或祖先未拒绝", err)
				}
			}
		}
		if countEntries() != before {
			t.Fatal("不可见父目录检查后仍创建了记录")
		}
	})
	t.Run("depth boundary", func(t *testing.T) {
		parentID := uint64(11)
		for depth := 1; depth <= 31; depth++ {
			name := fmt.Sprintf("depth%d", depth)
			key, err := service.BuildEntryNameKey(name)
			if err != nil {
				t.Fatal(err)
			}
			entryID := uint64(100 + depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", entryID, parentID, name, key); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		last, err := creator.Create(ctx, actor, parentID, "第32层", nil)
		if err != nil || last == nil {
			t.Fatal("合法第 32 层创建失败", err)
		}
		before := countEntries()
		if entry, err := creator.Create(ctx, actor, last.ID, "第33层", nil); entry != nil || !errors.Is(err, domain.ErrFileEntryDepthExceeded) || countEntries() != before {
			t.Fatal("超出层级上限仍创建目录", err)
		}
	})

	// 3. 操作故障和取消在提交前传播，实际插入记录随事务回滚。
	t.Run("invalid input and result rollback", func(t *testing.T) {
		before := countEntries()
		invalidColor := "green\n"
		for _, input := range []struct {
			name  string
			color *string
			want  error
		}{{"CON.txt", nil, domain.ErrInvalidEntryName}, {"a/b", nil, domain.ErrInvalidEntryName}, {"正常名称", &invalidColor, domain.ErrInvalidDirectoryColorKey}} {
			if entry, err := creator.Create(ctx, actor, 12, input.name, input.color); entry != nil || !errors.Is(err, input.want) {
				t.Fatal("无效输入仍进入创建流程", err)
			}
		}
		broken := service.NewCreateDirectoryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.CreateDirectoryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, &createDirectoryResultStore{Store: txStore, afterCreate: func(entry *domain.FileEntry) { entry.Revision = 2 }})
			})
		})
		if entry, err := broken.Create(ctx, actor, 12, "结果异常回滚", nil); entry != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("异常写入结果未使事务失败", err)
		}
		missing(t, "结果异常回滚")
		if countEntries() != before {
			t.Fatal("失败后保留了新记录")
		}
	})
	t.Run("cancellation and authorization recheck", func(t *testing.T) {
		operationContext, stop := context.WithCancel(ctx)
		defer stop()
		cancelledCreator := service.NewCreateDirectoryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.CreateDirectoryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, &createDirectoryResultStore{Store: txStore, afterCreate: func(*domain.FileEntry) { stop() }})
			})
		})
		if entry, err := cancelledCreator.Create(operationContext, actor, 12, "取消回滚", nil); entry != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("插入后取消未保留失败原因", err)
		}
		missing(t, "取消回滚")
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.SessionID); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=NULL WHERE id=?", actor.SessionID); err != nil {
				t.Error(err)
			}
		}()
		if entry, err := creator.Create(ctx, actor, 12, "旧身份创建", nil); entry != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("事务入口未复查旧 Actor 的会话", err)
		}
		missing(t, "旧身份创建")
	})
	t.Run("concurrent normalized names", func(t *testing.T) {
		results := make(chan error, 2)
		begin := make(chan struct{})
		var workers sync.WaitGroup
		for _, name := range []string{"Straße", "STRASSE"} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-begin
				entry, err := creator.Create(ctx, actor, 12, name, nil)
				if err == nil && entry == nil {
					err = errors.New("成功创建缺少目录结果")
				}
				results <- err
			}()
		}
		close(begin)
		workers.Wait()
		close(results)
		succeeded, conflicted := 0, 0
		for err := range results {
			if err == nil {
				succeeded++
			} else if errors.Is(err, domain.ErrEntryNameConflict) {
				conflicted++
			} else {
				t.Fatal("并发创建返回意外错误", err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatal("并发规范等价名称未得到一次成功和一次冲突")
		}
	})

	// 4. 高位自增调整放在所有普通插入之后，验证服务实际交付完整 ID。
	t.Run("unsigned generated id", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "ALTER TABLE file_entries AUTO_INCREMENT = 9223372036854775810"); err != nil {
			t.Fatal(err)
		}
		entry, err := creator.Create(ctx, actor, 11, "高位服务目录", nil)
		if err != nil || entry == nil || entry.ID < uint64(1)<<63 || entry.UserID != 1 || entry.ParentID == nil || *entry.ParentID != 11 {
			t.Fatal("服务未返回高位无符号自增 ID", err)
		}
	})
}
