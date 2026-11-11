package repository

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func userProfileDatabase(t *testing.T) *sql.DB {
	t.Helper()
	// 1. 沿用独占库与清理复核机制，替换该库中的最小事务测试表。
	pool := loginTransactionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{"DROP TABLE user_devices", "DROP TABLE users"} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	// 2. 从固定迁移提取四张正式表，满足根目录关联表的外键依赖。
	for _, boundary := range []struct{ table, next string }{
		{table: "users", next: "user_devices"},
		{table: "user_devices", next: "auth_sessions"},
		{table: "stored_objects", next: "file_entries"},
		{table: "file_entries", next: "user_entry_preferences"},
	} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		if start < 0 {
			t.Fatal("正式表定义未找到")
		}
		end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE "+boundary.next+" (")
		if end < 0 {
			t.Fatal("正式表定义结束边界未找到")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestFindUserProfileIntegration(t *testing.T) {
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status,storage_quota,used_storage,reserved_storage) VALUES (1,'fixture_one','资料用户 🌱','fixture_hash','ACTIVE',100,60,40),(2,'fixture_two','停用用户','fixture_hash','DISABLED',0,0,0),(3,'fixture_three','缺少根目录','fixture_hash','ACTIVE',100,0,0)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(12,1,11,'child',X'6368696C64','DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.UserProfile{ID: 1, Username: "fixture_one", DisplayName: "资料用户 🌱", Status: "ACTIVE", RootEntryID: 11, QuotaBytes: 100, UsedBytes: 60, ReservedBytes: 40}
	check := func(t *testing.T, queryContext context.Context, expected domain.UserProfile) {
		t.Helper()
		actual, err := store.FindUserProfile(queryContext, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatalf("用户资料读回不一致: %v", err)
		}
	}
	t.Run("real root and unicode profile", func(t *testing.T) {
		var before time.Time
		var beforeSeq uint64
		if err := pool.QueryRowContext(ctx, "SELECT updated_at,change_seq FROM users WHERE id=1").Scan(&before, &beforeSeq); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, want)
		check(t, ctx, want)
		var after time.Time
		var afterSeq uint64
		if err := pool.QueryRowContext(ctx, "SELECT updated_at,change_seq FROM users WHERE id=1").Scan(&after, &afterSeq); err != nil || !after.Equal(before) || beforeSeq != afterSeq {
			t.Fatal("查询修改了用户时间或同步序号")
		}
	})
	t.Run("disabled and zero quota", func(t *testing.T) {
		check(t, ctx, domain.UserProfile{ID: 2, Username: "fixture_two", DisplayName: "停用用户", Status: "DISABLED", RootEntryID: 22})
	})
	t.Run("missing account and missing root differ", func(t *testing.T) {
		if result, err := store.FindUserProfile(ctx, 99); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("不存在账号的结果错误")
		}
		if result, err := store.FindUserProfile(ctx, 3); result != nil || !errors.Is(err, domain.ErrInvalidAccountData) {
			t.Fatal("缺少根目录时误用了其他用户根目录或未报告数据异常")
		}
		var roots int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries WHERE user_id=3").Scan(&roots); err != nil || roots != 0 {
			t.Fatal("只读查询创建了缺失根目录")
		}
	})
	t.Run("consistent read while user row locked", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE users SET display_name='提交后的资料',storage_quota=200,used_storage=150,reserved_storage=50 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		readContext, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		check(t, readContext, want)
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		want.DisplayName, want.QuotaBytes, want.UsedBytes, want.ReservedBytes = "提交后的资料", 200, 150, 50
		check(t, ctx, want)
	})
	// 最大自增 ID 放在所有普通夹具建立之后，随后只读或更新已有记录。
	t.Run("full unsigned bigint range", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage) VALUES(?,'fixture_max','完整整数','fixture_hash',?,?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		profile := domain.UserProfile{ID: maximum, Username: "fixture_max", DisplayName: "完整整数", Status: "ACTIVE", RootEntryID: maximum, QuotaBytes: maximum, UsedBytes: maximum}
		check(t, ctx, profile)
		if _, err := pool.ExecContext(ctx, "UPDATE users SET used_storage=0,reserved_storage=? WHERE id=?", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		profile.UsedBytes, profile.ReservedBytes = 0, maximum
		check(t, ctx, profile)
	})
	t.Run("closed pool is an error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := store.FindUserProfile(ctx, 1); result != nil || err == nil || errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("连接池故障被当作账号未找到")
		}
	})
}
