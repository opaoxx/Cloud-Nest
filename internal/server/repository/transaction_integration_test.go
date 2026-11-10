package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func loginTransactionDatabase(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("CLOUDNEST_LOGIN_TX_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_LOGIN_TX_TEST=1 后自动创建并清理独占事务验证库")
	}
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := OpenMySQL(context.Background(), databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })

	// 1. 新库名只用本用例前缀和时间数字；创建成功后才登记删除，避免误删已有库。
	const prefix = "cloudnest_login_tx_"
	databaseName := prefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	setupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := adminPool.ExecContext(setupContext, "CREATE DATABASE `"+databaseName+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !strings.HasPrefix(databaseName, prefix) {
			t.Error("拒绝清理非本用例的测试库")
			return
		}
		if _, err := strconv.ParseUint(strings.TrimPrefix(databaseName, prefix), 10, 64); err != nil {
			t.Error("拒绝清理格式不符的库名")
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminPool.ExecContext(cleanupContext, "DROP DATABASE `"+databaseName+"`"); err != nil {
			t.Errorf("清理隔离事务库失败: %v", err)
			return
		}
		var remaining int
		if err := adminPool.QueryRowContext(cleanupContext, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", databaseName).Scan(&remaining); err != nil || remaining != 0 {
			t.Errorf("隔离库清理复核失败 count=%d error=%v", remaining, err)
		}
	})
	databaseConfig.DatabaseName = databaseName
	pool, err := OpenMySQL(context.Background(), databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	// 2. 仅创建事务测试最小表，probe_value 用于观察提交/回滚，不是业务迁移。
	for _, statement := range []string{
		"CREATE TABLE users (id BIGINT UNSIGNED PRIMARY KEY, username VARCHAR(64), password_hash VARCHAR(255), status VARCHAR(16) NOT NULL, probe_value INT NOT NULL DEFAULT 0) ENGINE=InnoDB",
		"CREATE TABLE user_devices (id BIGINT UNSIGNED PRIMARY KEY, user_id BIGINT UNSIGNED NOT NULL, installation_id CHAR(36) NOT NULL, name VARCHAR(100) NOT NULL, platform VARCHAR(16) NOT NULL, revoked_at DATETIME(6) NULL) ENGINE=InnoDB",
		"INSERT INTO users(id,username,password_hash,status) VALUES(1,'fixture_one','fixture_hash','ACTIVE'),(2,'fixture_two','fixture_hash','DISABLED'),(3,'fixture_three','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices VALUES(11,1,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB',NULL)",
	} {
		if _, err := pool.ExecContext(setupContext, statement); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestWithLoginUserTransactionIntegration(t *testing.T) {
	pool := loginTransactionDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	readProbe := func() int {
		var value int
		if err := pool.QueryRow("SELECT probe_value FROM users WHERE id=1").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	writeProbe := func(txContext context.Context, txStore *Store) error {
		return txStore.database.WithContext(txContext).Exec("UPDATE users SET probe_value=probe_value+1 WHERE id=1").Error
	}
	t.Run("commit and repository reuse", func(t *testing.T) {
		// 池最多一条连接，回调若错误使用外部 Store 会无法借到连接。
		pool.SetMaxOpenConns(1)
		err := store.WithLoginUserTransaction(context.Background(), 1, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.FindLoginDeviceByInstallation(txContext, 1, "28ee758a-0727-42de-ae7d-3bb33155e148")
			if err != nil {
				return err
			}
			if device == nil || device.ID != 11 {
				return fmt.Errorf("事务内设备查询不正确")
			}
			return writeProbe(txContext, txStore)
		})
		if err != nil || readProbe() != 1 {
			t.Fatalf("提交或事务内 Repository 复用失败: %v", err)
		}
	})
	t.Run("callback failure rollback", func(t *testing.T) {
		failure := errors.New("fixture business failure")
		err := store.WithLoginUserTransaction(context.Background(), 1, func(txContext context.Context, txStore *Store) error {
			if err := writeProbe(txContext, txStore); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) || readProbe() != 1 {
			t.Fatal("失败回调的写入没有回滚")
		}
	})
	t.Run("panic rollback", func(t *testing.T) {
		func() {
			defer func() {
				if recover() != "fixture panic" {
					t.Error("panic 未按原值传播")
				}
			}()
			_ = store.WithLoginUserTransaction(context.Background(), 1, func(txContext context.Context, txStore *Store) error {
				if err := writeProbe(txContext, txStore); err != nil {
					t.Error(err)
					return err
				}
				panic("fixture panic")
			})
		}()
		if readProbe() != 1 {
			t.Fatal("panic 路径没有回滚写入")
		}
	})
	t.Run("disabled and missing never invoke callback", func(t *testing.T) {
		for _, testCase := range []struct {
			userID uint64
			want   error
		}{{userID: 2, want: domain.ErrAccountDisabled}, {userID: 999, want: domain.ErrAccountNotFound}} {
			err := store.WithLoginUserTransaction(context.Background(), testCase.userID, func(context.Context, *Store) error { t.Error("不允许的用户执行了回调"); return nil })
			if !errors.Is(err, testCase.want) {
				t.Fatalf("账号复查结果不正确: %v", err)
			}
		}
	})
	t.Run("same user lock and current status", func(t *testing.T) {
		pool.SetMaxOpenConns(3)
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseFirst()
		firstResult := make(chan error, 1)
		go func() {
			firstResult <- store.WithLoginUserTransaction(context.Background(), 3, func(txContext context.Context, txStore *Store) error {
				close(entered)
				<-release
				return txStore.database.WithContext(txContext).Exec("UPDATE users SET status='DISABLED' WHERE id=3").Error
			})
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("第一个用户事务没有取得锁")
		}

		// 先用短期限确认第二个连接确实被同一用户行锁阻塞。
		shortContext, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		err := store.WithLoginUserTransaction(shortContext, 3, func(context.Context, *Store) error { t.Error("未等到用户锁便进入回调"); return nil })
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("用户行锁没有阻止并发写入: %v", err)
		}

		// 再等待前一个事务提交禁用状态，锁定读取必须复查出新状态。
		secondResult := make(chan error, 1)
		var secondCalled atomic.Bool
		go func() {
			secondResult <- store.WithLoginUserTransaction(context.Background(), 3, func(context.Context, *Store) error { secondCalled.Store(true); return nil })
		}()
		select {
		case err := <-secondResult:
			t.Fatalf("首个事务未结束便完成第二个事务: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		releaseFirst()
		select {
		case err := <-firstResult:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("首个事务未结束")
		}
		select {
		case err := <-secondResult:
			if !errors.Is(err, domain.ErrAccountDisabled) || secondCalled.Load() {
				t.Fatal("等待锁后未复查到最新禁用状态")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等待锁的事务没有结束")
		}
	})
	if err := pool.PingContext(context.Background()); err != nil {
		t.Fatal("事务入口擅自关闭了共享连接池")
	}
}
