package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestRecordUserEntryAccessIntegration(t *testing.T) {
	// 1. 正式表独占库准备身份、文件及不同偏好状态。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"user_entry_preferences", "upload_tasks"}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("正式表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'access_one','访问用户','fixture_hash','ACTIVE'),(3,'access_other','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,1,'28ee758a-0727-42de-ae7d-3bb33155e148','访问设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,19,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','access-entry',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'访问父目录',CAST('访问父目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'首次.md',CAST('首次.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(14,1,12,'已收藏.md',CAST('已收藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(15,1,12,'旧访问.md',CAST('旧访问.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at,created_at,updated_at) VALUES(1,14,?,NULL,?,?),(1,15,?,?,?,?)", old, old, old, old, old, old, old); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	record := func(callContext context.Context, entryID uint64) (*domain.EntryPreference, error) {
		var prepared *domain.EntryPreference
		err := store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
			paths := service.NewFileEntryPathService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
				return action(ctx, txStore)
			})
			if _, err := paths.Get(txContext, actor, entryID); err != nil {
				return err
			}
			var err error
			prepared, err = txStore.RecordUserEntryAccess(txContext, entryID)
			return err
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}
	read := func(entryID uint64) *domain.EntryPreference {
		t.Helper()
		preference, err := store.FindUserEntryPreference(ctx, 1, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return preference
	}
	audit := func(entryID uint64) [2]time.Time {
		t.Helper()
		var stamps [2]time.Time
		if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=?", entryID).Scan(&stamps[0], &stamps[1]); err != nil {
			t.Fatal(err)
		}
		return stamps
	}

	// 2. 单连接完成首次与窗口内访问，核对收藏、审计及基础事实。
	t.Run("single connection first and repeat", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		before, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil {
			t.Fatal(err)
		}
		first, err := record(ctx, 13)
		if err != nil || first == nil || first.LastOpenedAt == nil || first.StarredAt != nil || first.LastOpenedAt.Location() != time.UTC || first.LastOpenedAt.Nanosecond()%1000 != 0 {
			t.Fatal("首次访问时间错误", err)
		}
		stamp, stamps := *first.LastOpenedAt, audit(13)
		*first.LastOpenedAt = old
		repeated, err := record(ctx, 13)
		if err != nil || repeated == nil || !repeated.LastOpenedAt.Equal(stamp) || audit(13) != stamps {
			t.Fatal("窗口内访问或审计时间错误", err)
		}
		after, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("访问与基础事实混用", err)
		}
	})
	t.Run("existing preference and star preservation", func(t *testing.T) {
		for _, entryID := range []uint64{14, 15} {
			before := audit(entryID)
			preference, err := record(ctx, entryID)
			if err != nil || preference == nil || preference.StarredAt == nil || !preference.StarredAt.Equal(old) || preference.LastOpenedAt == nil || !preference.LastOpenedAt.After(old) {
				t.Fatal("原收藏或新增访问时间错误", err)
			}
			after := audit(entryID)
			if before[0] != after[0] || !after[1].After(before[1]) {
				t.Fatal("偏好审计时间错误")
			}
			*preference.StarredAt, *preference.LastOpenedAt = old.Add(time.Hour), old
			current := read(entryID)
			if !current.StarredAt.Equal(old) || !current.LastOpenedAt.After(old) {
				t.Fatal("返回指针影响数据库偏好")
			}
		}
	})

	// 3. 仅在测试事务连接固定数据库时钟，精确核对微秒级窗口边界。
	t.Run("exact sixty second boundaries", func(t *testing.T) {
		var fixed time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&fixed); err != nil {
			t.Fatal(err)
		}
		fixed = fixed.Truncate(time.Second)
		for _, testCase := range []struct {
			name   string
			age    time.Duration
			update bool
		}{
			{"one microsecond inside", 60*time.Second - time.Microsecond, false},
			{"exactly sixty seconds", 60 * time.Second, true},
			{"one microsecond outside", 60*time.Second + time.Microsecond, true},
			{"future fact", -time.Hour, false},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				previous := fixed.Add(-testCase.age)
				var preference *domain.EntryPreference
				err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					if err := txStore.database.WithContext(txContext).Exec("SET timestamp = ?", fixed.Unix()).Error; err != nil {
						return err
					}
					setupError := txStore.database.WithContext(txContext).Exec("UPDATE user_entry_preferences SET starred_at=?,last_opened_at=?,updated_at=? WHERE user_id=1 AND entry_id=13", old, previous, old).Error
					var writeError error
					if setupError == nil {
						preference, writeError = txStore.RecordUserEntryAccess(txContext, 13)
					}
					resetError := txStore.database.WithContext(txContext).Exec("SET timestamp = DEFAULT").Error
					return errors.Join(setupError, writeError, resetError)
				})
				wantOpened, wantUpdated := previous, old
				if testCase.update {
					wantOpened, wantUpdated = fixed, fixed
				}
				if err != nil || preference == nil || preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(wantOpened) || !preference.StarredAt.Equal(old) || !audit(13)[1].Equal(wantUpdated) {
					t.Fatal("六十秒精确边界错误", err)
				}
			})
		}
	})
	t.Run("scope type and hidden paths", func(t *testing.T) {
		for _, testCase := range []struct {
			id   uint64
			want error
		}{{33, domain.ErrFileEntryNotFound}, {999, domain.ErrFileEntryNotFound}, {11, domain.ErrFileEntryNotFile}, {12, domain.ErrFileEntryNotFile}} {
			result, err := record(ctx, testCase.id)
			if result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("访问范围或文件类型错误", err)
			}
		}
		before := read(13)
		for _, entryID := range []uint64{12, 13} {
			for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?='PURGED',NULL,object_id) WHERE id=?", state, state, state, state, entryID); err != nil {
					t.Fatal(err)
				}
				result, operationError := record(ctx, 13)
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", entryID); err != nil {
					t.Fatal(err)
				}
				if result != nil || !errors.Is(operationError, domain.ErrFileEntryNotFound) || !reflect.DeepEqual(before, read(13)) {
					t.Fatal("隐藏路径访问错误", operationError)
				}
			}
		}
	})

	// 4. 实际写入后失败或取消回滚，认证状态变化由事务复查。
	t.Run("rollback after write", func(t *testing.T) {
		for _, cancelAfter := range []bool{false, true} {
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=?,updated_at=? WHERE user_id=1 AND entry_id=13", old, old); err != nil {
				t.Fatal(err)
			}
			before, stamps := read(13), audit(13)
			callContext, cancelCall := context.WithCancel(ctx)
			failure := errors.New("fixture access rollback")
			err := store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.RecordUserEntryAccess(txContext, 13); err != nil {
					return err
				}
				if cancelAfter {
					cancelCall()
					return txContext.Err()
				}
				return failure
			})
			cancelCall()
			want := failure
			if cancelAfter {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(before, read(13)) || audit(13) != stamps {
				t.Fatal("访问写后回滚错误", err)
			}
		}
	})
	t.Run("identity recheck", func(t *testing.T) {
		for _, item := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=19", "UPDATE user_devices SET revoked_at=NULL WHERE id=19", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			before := read(13)
			if _, err := pool.ExecContext(ctx, item.change); err != nil {
				t.Fatal(err)
			}
			result, operationError := record(ctx, 13)
			if _, err := pool.ExecContext(ctx, item.restore); err != nil {
				t.Fatal(err)
			}
			if result != nil || !errors.Is(operationError, item.want) || !reflect.DeepEqual(before, read(13)) {
				t.Fatal("访问身份复查错误", operationError)
			}
		}
	})
	// 5. 旧快照后当前读回与并发访问复用同一数据库事实。
	t.Run("current read over old snapshot", func(t *testing.T) {
		var current *domain.EntryPreference
		newOpened := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.FindUserEntryPreference(txContext, 1, 13); err != nil {
				return err
			}
			if _, err := pool.ExecContext(txContext, "UPDATE user_entry_preferences SET starred_at=?,last_opened_at=?,updated_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=13", old, newOpened); err != nil {
				return err
			}
			var err error
			current, err = txStore.RecordUserEntryAccess(txContext, 13)
			return err
		})
		if err != nil || current == nil || !current.LastOpenedAt.Equal(newOpened) || !current.StarredAt.Equal(old) {
			t.Fatal("当前偏好读取错误", err)
		}
	})
	t.Run("concurrent accesses", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=? WHERE user_id=1 AND entry_id=13", old); err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			preference *domain.EntryPreference
			err        error
		}
		results := make(chan outcome, 2)
		start := make(chan struct{})
		for range 2 {
			go func() { <-start; preference, err := record(ctx, 13); results <- outcome{preference, err} }()
		}
		close(start)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil || first.preference == nil || second.preference == nil || first.preference.LastOpenedAt == nil || !reflect.DeepEqual(first.preference, second.preference) || !first.preference.LastOpenedAt.After(old) {
			t.Fatal("并发访问事实错误", first.err, second.err)
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=1 AND entry_id=13").Scan(&count); err != nil || count != 1 {
			t.Fatal("并发偏好数量错误", err)
		}
	})
	t.Run("concurrent star and access", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=? WHERE user_id=1 AND entry_id=13", old); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 2)
		start := make(chan struct{})
		go func() {
			<-start
			_, err := record(ctx, 13)
			results <- err
		}()
		go func() {
			<-start
			results <- store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				_, err := txStore.SetUserEntryStarred(txContext, 13, true)
				return err
			})
		}()
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		preference := read(13)
		if preference.StarredAt == nil || preference.LastOpenedAt == nil || !preference.LastOpenedAt.After(old) {
			t.Fatal("并发收藏与访问事实错误")
		}
	})
	t.Run("maximum identifiers revision and dates", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(?,'max_access','最大访问用户','fixture_hash','ACTIVE')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'ca67141f-9801-40be-aa25-d7cb9cdc74e6','最大设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','maximum-access',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(?,?,NULL,'',X'','DIR',NULL,NULL,0,1),(?,?,?,'最大文件',CAST('最大文件' AS BINARY),'FILE','OTHER',?,0,?)", []any{maximum - 1, maximum, maximum, maximum, maximum - 1, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		maximumActor := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		var result *domain.EntryPreference
		invoke := func() error {
			return store.WithUserTransaction(ctx, maximumActor, func(txContext context.Context, txStore *Store) error {
				var err error
				result, err = txStore.RecordUserEntryAccess(txContext, maximum)
				return err
			})
		}
		if err := invoke(); err != nil || result == nil || result.UserID != maximum || result.EntryID != maximum || result.LastOpenedAt == nil {
			t.Fatal("最大标识访问错误", err)
		}
		minimumStar, maximumOpened := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=?,last_opened_at=? WHERE user_id=? AND entry_id=?", minimumStar, maximumOpened, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if err := invoke(); err != nil || result.StarredAt == nil || !result.StarredAt.Equal(minimumStar) || !result.LastOpenedAt.Equal(maximumOpened) {
			t.Fatal("日期边界或未来访问事实错误", err)
		}
	})
}
