package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateUserDirectoryIntegration(t *testing.T) {
	// 1. 在独占库中复用正式目录项表，再安装认证事务依赖的正式会话表。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表迁移边界错误")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','目录创建用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','创建目录设备','WEB'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','其他用户设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(15,1,11,'父目录',CAST('父目录' AS BINARY),'DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(12,1,11,'file.txt',CAST('file.txt' AS BINARY),'FILE','DOCUMENT',41,2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收父目录',CAST('回收父目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(18,1,11,'清理父目录',CAST('清理父目录' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purged_at) VALUES(19,1,11,'已清理父目录',CAST('已清理父目录' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立目录创建夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	keyFor := func(name string) []byte {
		t.Helper()
		key, err := service.BuildEntryNameKey(name)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	create := func(createContext context.Context, current domain.Actor, parentID uint64, rawName string, color *string) (*domain.FileEntry, error) {
		name, err := service.ValidateEntryName(rawName)
		if err != nil {
			return nil, err
		}
		key, err := service.BuildEntryNameKey(name)
		if err != nil {
			return nil, err
		}
		var prepared *domain.FileEntry
		err = store.WithUserTransaction(createContext, current, func(txContext context.Context, txStore *Store) error {
			var err error
			prepared, err = txStore.CreateUserDirectory(txContext, parentID, name, key, color)
			return err
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}
	checkMissing := func(t *testing.T, name string) {
		t.Helper()
		if entry, err := store.FindUserActiveFileEntryByName(ctx, 1, 11, keyFor(name)); entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("回滚后仍存在活动目录", err)
		}
	}

	// 2. 单连接创建验证事务句柄复用，并独立检查数据库默认值及颜色空值。
	t.Run("defaults color and one connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		color := "green"
		entry, err := create(ctx, actor, 11, "  新目录 🌱  ", &color)
		if err != nil || entry == nil || entry.ID == 0 || entry.UserID != 1 || entry.ParentID == nil || *entry.ParentID != 11 || entry.Name != "新目录 🌱" || entry.EntryType != "DIR" || entry.FileSize != 0 || entry.Revision != 1 || entry.Status != "ACTIVE" {
			t.Fatal("创建目录的基础记录错误", err)
		}
		if entry.CreatedAt.IsZero() || entry.CreatedAt.Location() != time.UTC || !entry.CreatedAt.Equal(entry.ModifiedAt) || !entry.CreatedAt.Equal(entry.UpdatedAt) || entry.CreatedAt.Nanosecond()%1000 != 0 {
			t.Fatal("数据库默认 UTC 微秒时间错误")
		}
		var storedKey, activeKey []byte
		var objectID, rootOwner sql.NullInt64
		var deletedAt, purgedAt sql.NullTime
		if err := pool.QueryRowContext(ctx, "SELECT name_key,active_name_key,object_id,root_owner_id,deleted_at,purged_at FROM file_entries WHERE id=?", entry.ID).Scan(&storedKey, &activeKey, &objectID, &rootOwner, &deletedAt, &purgedAt); err != nil || !bytes.Equal(storedKey, keyFor(entry.Name)) || !bytes.Equal(activeKey, storedKey) || objectID.Valid || rootOwner.Valid || deletedAt.Valid || purgedAt.Valid {
			t.Fatal("生成键、对象引用或生命周期默认值错误", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, entry.ID)
		if err != nil || display.ColorKey == nil || *display.ColorKey != "green" || display.Category != nil || display.Extension != nil || display.MIMEType != nil || display.SourceDeviceID != nil || display.SourceDeviceName != nil || display.ContentModifiedAt != nil {
			t.Fatal("目录颜色或可空内容字段错误", err)
		}
		*entry.ParentID = 999
		if again, err := store.FindUserFileEntry(ctx, 1, entry.ID); err != nil || again.ParentID == nil || *again.ParentID != 11 {
			t.Fatal("修改返回副本影响了数据库", err)
		}
	})
	t.Run("conflicts rollback and historical reuse", func(t *testing.T) {
		original, err := create(ctx, actor, 11, "Café.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		if entry, err := create(ctx, actor, 11, "CAFE\u0301.TXT", nil); entry != nil || !errors.Is(err, domain.ErrEntryNameConflict) || strings.Contains(err.Error(), "Duplicate entry") || strings.Contains(strings.ToLower(err.Error()), "café") {
			t.Fatal("等价名称未转换为固定重名错误", err)
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.CreateUserDirectory(txContext, 11, "先写后冲突", keyFor("先写后冲突"), nil); err != nil {
				return err
			}
			_, err := txStore.CreateUserDirectory(txContext, 11, "CAFE\u0301.TXT", keyFor("CAFE\u0301.TXT"), nil)
			return err
		})
		if !errors.Is(err, domain.ErrEntryNameConflict) {
			t.Fatal(err)
		}
		checkMissing(t, "先写后冲突")
		if _, err := create(ctx, actor, 15, "Café.txt", nil); err != nil {
			t.Fatal("其他父目录不能使用相同名称", err)
		}
		other := domain.Actor{UserID: 3, DeviceID: 31, SessionID: 37}
		if entry, err := create(ctx, other, 33, "Café.txt", nil); err != nil || entry.UserID != 3 {
			t.Fatal("其他用户不能使用相同名称", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=?", original.ID); err != nil {
			t.Fatal(err)
		}
		if replacement, err := create(ctx, actor, 11, "CAFE\u0301.TXT", nil); err != nil || replacement.ID == original.ID || replacement.Revision != 1 {
			t.Fatal("历史同名项阻止重新创建或被覆盖", err)
		}
	})
	t.Run("unicode expansion and nullable colors", func(t *testing.T) {
		empty, sand := "", "sand"
		for _, testCase := range []struct {
			name  string
			color *string
		}{
			{name: "Straße", color: &sand},
			{name: strings.Repeat("🌱", 60)},
			{name: strings.Repeat("\u0f73", 120), color: &empty},
		} {
			entry, err := create(ctx, actor, 11, testCase.name, testCase.color)
			if err != nil || entry.Name != testCase.name {
				t.Fatal("Unicode 展示名或 UTF-16 上限创建错误", err)
			}
			var key []byte
			var color sql.NullString
			if err := pool.QueryRowContext(ctx, "SELECT name_key,color_key FROM file_entries WHERE id=?", entry.ID).Scan(&key, &color); err != nil || !bytes.Equal(key, keyFor(testCase.name)) || color.Valid != (testCase.color != nil) || (testCase.color != nil && color.String != *testCase.color) {
				t.Fatal("展开名称键或颜色空值保存错误", err)
			}
		}
		tooLongColor := strings.Repeat("a", 17)
		if entry, err := create(ctx, actor, 11, "颜色约束失败", &tooLongColor); entry != nil || err == nil || errors.Is(err, domain.ErrEntryNameConflict) {
			t.Fatal("数据库颜色约束故障被当作成功或重名", err)
		}
		checkMissing(t, "颜色约束失败")
	})

	// 3. 父目录范围检查先于 INSERT，其他用户的父目录锁也应被直接避开。
	t.Run("parent ownership type state and foreign lock", func(t *testing.T) {
		for _, testCase := range []struct {
			parentID uint64
			wantErr  error
		}{
			{999, domain.ErrFileEntryNotFound}, {33, domain.ErrFileEntryNotFound},
			{12, domain.ErrFileEntryNotDirectory}, {16, domain.ErrFileEntryNotFound},
			{18, domain.ErrFileEntryNotFound}, {19, domain.ErrFileEntryNotFound},
		} {
			if entry, err := create(ctx, actor, testCase.parentID, "父目录失败", nil); entry != nil || !errors.Is(err, testCase.wantErr) {
				t.Fatal("父目录异常未拒绝创建", err)
			}
		}
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET revision=revision+1 WHERE id=33"); err != nil {
			t.Fatal(err)
		}
		shortContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		if entry, err := create(shortContext, actor, 33, "外部父目录", nil); entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("跨账号父目录查询等待了其他用户行锁", err)
		}
	})
	t.Run("outer failure cancellation and ended transaction", func(t *testing.T) {
		failure := errors.New("fixture outer rollback")
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.CreateUserDirectory(txContext, 11, "外层失败", keyFor("外层失败"), nil); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		checkMissing(t, "外层失败")
		operationContext, stop := context.WithCancel(ctx)
		defer stop()
		err = store.WithUserTransaction(operationContext, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.CreateUserDirectory(txContext, 11, "提交前取消", keyFor("提交前取消"), nil); err != nil {
				return err
			}
			stop()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		checkMissing(t, "提交前取消")
		var ended *Store
		if err := store.WithUserTransaction(ctx, actor, func(_ context.Context, txStore *Store) error {
			ended = txStore
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if entry, err := ended.CreateUserDirectory(ctx, 11, "结束后创建", keyFor("结束后创建"), nil); entry != nil || !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("已结束事务仍允许创建目录", err)
		}
	})

	// 4. 当前身份状态在事务入口复查，同名并发写入通过用户锁及唯一键收敛。
	t.Run("authorization state rechecked", func(t *testing.T) {
		for _, testCase := range []struct {
			name, change, restore string
			wantErr               error
		}{
			{"disabled", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"device revoked", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"session revoked", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
			{"session expired", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				if _, err := pool.ExecContext(ctx, testCase.change); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := pool.ExecContext(ctx, testCase.restore); err != nil {
						t.Error("恢复认证夹具失败", err)
					}
				}()
				if entry, err := create(ctx, actor, 11, "状态拒绝创建", nil); entry != nil || !errors.Is(err, testCase.wantErr) {
					t.Fatal("事务入口未拒绝失效调用者", err)
				}
			})
		}
		checkMissing(t, "状态拒绝创建")
	})
	t.Run("concurrent equivalent names", func(t *testing.T) {
		type outcome struct {
			entry *domain.FileEntry
			err   error
		}
		results := make(chan outcome, 2)
		begin := make(chan struct{})
		var workers sync.WaitGroup
		for _, name := range []string{"Concurrent", "CONCURRENT"} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-begin
				entry, err := create(ctx, actor, 11, name, nil)
				results <- outcome{entry: entry, err: err}
			}()
		}
		close(begin)
		workers.Wait()
		close(results)
		succeeded, conflicted := 0, 0
		for result := range results {
			if result.err == nil && result.entry != nil {
				succeeded++
			} else if result.entry == nil && errors.Is(result.err, domain.ErrEntryNameConflict) {
				conflicted++
			} else {
				t.Fatal("同名并发产生意外结果", result.err)
			}
		}
		if succeeded != 1 || conflicted != 1 {
			t.Fatal("同名并发未得到一次成功和一次名称冲突")
		}
	})

	// 5. 高位自增值放在普通夹具之后，验证完整无符号 ID 的实际生成及回读。
	t.Run("unsigned owner parent and generated id", func(t *testing.T) {
		maximum := ^uint64(0)
		parentID := maximum - 10
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整整数设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", parentID, maximum); err != nil {
			t.Fatal(err)
		}
		fullActor := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		entry, err := create(ctx, fullActor, parentID, "高位自增目录", nil)
		if err != nil || entry == nil || entry.ID <= parentID || entry.UserID != maximum || entry.ParentID == nil || *entry.ParentID != parentID || entry.Revision != 1 {
			t.Fatal("高位无符号自增 ID 创建或读回错误", err)
		}
	})
}
