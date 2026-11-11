package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUserProfileServiceIntegration(t *testing.T) {
	// 1. 复用正式用户和根目录表的独占库，绑定真实查询方法。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage,reserved_storage) VALUES(1,'fixture_one','资料服务用户','fixture_hash',100,60,40),(2,'fixture_two','缺少根目录','fixture_hash',100,0,0)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	profiles := service.NewUserProfileService(store.FindUserProfile)
	want := &domain.UserProfile{ID: 1, Username: "fixture_one", DisplayName: "资料服务用户", Status: "ACTIVE", RootEntryID: 11, QuotaBytes: 100, UsedBytes: 60, ReservedBytes: 40}
	checkActive := func(t *testing.T) {
		t.Helper()
		result, err := profiles.Get(ctx, 1)
		if err != nil || !reflect.DeepEqual(result, want) {
			t.Fatal("用户资料服务未返回真实账号、根目录和配额")
		}
	}
	t.Run("active profile", checkActive)

	// 2. 数据库提交禁用后，每次调用重新查询并拒绝，再启用后正常返回。
	t.Run("committed account status is rechecked", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if result, err := profiles.Get(ctx, 1); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("数据库账号禁用后仍返回了资料")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		checkActive(t)
	})
	t.Run("missing user and missing root", func(t *testing.T) {
		if result, err := profiles.Get(ctx, 99); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("真实账号未找到结果错误")
		}
		if result, err := profiles.Get(ctx, 2); result != nil || !errors.Is(err, domain.ErrInvalidAccountData) {
			t.Fatal("根目录数据异常未保留原因")
		}
	})
	t.Run("closed pool is a service error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := profiles.Get(ctx, 1); result != nil || err == nil || errors.Is(err, domain.ErrAccountNotFound) || errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("数据库故障被当作用户业务状态")
		}
	})
}
