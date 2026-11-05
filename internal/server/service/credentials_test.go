package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"golang.org/x/crypto/bcrypt"
)

func TestCredentialServiceOutcomesAndOrder(t *testing.T) {
	// 1. 以查询/验证替身观察编排顺序，避免测试结果依赖 bcrypt 运行耗时。
	queryFailure := errors.New("database unavailable")
	for _, testCase := range []struct {
		name                                                           string
		account                                                        *domain.LoginAccount
		queryError, verificationError, wantError                       error
		matched, wantVerify, cancelAfterQuery, cancelAfterVerification bool
		wantID                                                         uint64
	}{
		{name: "active correct password", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "ACTIVE"}, matched: true, wantVerify: true, wantID: 1},
		{name: "active wrong password", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "ACTIVE"}, wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "missing account", queryError: domain.ErrAccountNotFound, wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "wrapped missing account", queryError: fmt.Errorf("lookup: %w", domain.ErrAccountNotFound), wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "dummy match cannot authenticate", queryError: domain.ErrAccountNotFound, matched: true, wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "partial missing account ignored", account: &domain.LoginAccount{ID: 99, PasswordHash: "private-partial", Status: "ACTIVE"}, queryError: domain.ErrAccountNotFound, matched: true, wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "disabled correct password", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "DISABLED"}, matched: true, wantVerify: true, wantError: domain.ErrAccountDisabled},
		{name: "disabled wrong password", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "DISABLED"}, wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "query failure", queryError: queryFailure, wantError: queryFailure},
		{name: "partial query failure", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "ACTIVE"}, queryError: queryFailure, wantError: queryFailure},
		{name: "nil account without error", wantError: domain.ErrInvalidAccountData},
		{name: "invalid stored hash", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "ACTIVE"}, verificationError: domain.ErrInvalidPasswordHash, wantVerify: true, wantError: domain.ErrInvalidPasswordHash},
		{name: "invalid dummy hash reported as internal", queryError: domain.ErrAccountNotFound, verificationError: domain.ErrInvalidPasswordHash, wantVerify: true, wantError: domain.ErrInvalidPasswordHash},
		{name: "unknown state correct password", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "private-state"}, matched: true, wantVerify: true, wantError: domain.ErrInvalidAccountData},
		{name: "unknown state wrong password", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "private-state"}, wantVerify: true, wantError: domain.ErrInvalidCredentials},
		{name: "zero id cannot pass", account: &domain.LoginAccount{PasswordHash: "private-hash", Status: "ACTIVE"}, matched: true, wantVerify: true, wantError: domain.ErrInvalidAccountData},
		{name: "query cancellation stops verification", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "ACTIVE"}, cancelAfterQuery: true, wantError: context.Canceled},
		{name: "query cancellation precedes query error", queryError: queryFailure, cancelAfterQuery: true, wantError: context.Canceled},
		{name: "canceled computation cannot return identity", account: &domain.LoginAccount{ID: 1, PasswordHash: "private-hash", Status: "ACTIVE"}, matched: true, wantVerify: true, cancelAfterVerification: true, wantError: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var order []string
			var original domain.LoginAccount
			if testCase.account != nil {
				original = *testCase.account
			}
			credentialService := NewCredentialService(func(received context.Context, username string) (*domain.LoginAccount, error) {
				order = append(order, "query")
				if received != ctx || username != "private_user" {
					t.Error("查询没有收到原始上下文和用户名")
				}
				if testCase.cancelAfterQuery {
					cancel()
				}
				return testCase.account, testCase.queryError
			})
			credentialService.verifyPassword = func(hash, password string) (bool, error) {
				order = append(order, "verify")
				wantHash := missingAccountHash
				if !errors.Is(testCase.queryError, domain.ErrAccountNotFound) && testCase.account != nil {
					wantHash = testCase.account.PasswordHash
				}
				if hash != wantHash || password != " private-password " {
					t.Error("摘要选择或原始密码传递错误")
				}
				if testCase.cancelAfterVerification {
					cancel()
				}
				return testCase.matched, testCase.verificationError
			}
			identity, err := credentialService.Authenticate(ctx, "private_user", " private-password ")

			// 2. 错误不得返回身份；缺账号与错密码使用同一个固定错误，不泄漏账号数据。
			if testCase.wantError != nil {
				if identity != nil || !errors.Is(err, testCase.wantError) {
					t.Fatalf("结果错误 identity=%v err=%v", identity, err)
				}
				if testCase.wantError == domain.ErrInvalidCredentials && err != domain.ErrInvalidCredentials {
					t.Fatal("凭证错误未统一为同一个结果")
				}
				if strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "private_user") {
					t.Fatal("错误追加了用户名、密码、摘要或内部状态")
				}
			} else {
				if err != nil || identity == nil || identity.ID != testCase.wantID {
					t.Fatal("正确凭证未返回最小身份")
				}
				encoded, err := json.Marshal(identity)
				if err != nil || strings.Contains(string(encoded), "PasswordHash") || strings.Contains(string(encoded), "private-") {
					t.Fatal("成功身份带出了密码摘要")
				}
			}
			wantOrder := "query"
			if testCase.wantVerify {
				wantOrder = "query,verify"
			}
			if strings.Join(order, ",") != wantOrder {
				t.Fatalf("调用顺序=%v", order)
			}
			if testCase.account != nil && *testCase.account != original {
				t.Fatal("服务修改了查询返回的账号")
			}
		})
	}
}

func TestCredentialServiceConstructionAndContext(t *testing.T) {
	// 1. 依赖缺失是构造错误；正常构造只保存能力，不查询或计算密码。
	t.Run("nil query rejected", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺少查询能力未被拒绝")
			}
		}()
		NewCredentialService(nil)
	})
	queryCalls := 0
	credentialService := NewCredentialService(func(context.Context, string) (*domain.LoginAccount, error) {
		queryCalls++
		return nil, domain.ErrAccountNotFound
	})
	if queryCalls != 0 || credentialService.verifyPassword == nil {
		t.Fatal("构造阶段执行了查询或没有绑定比对方法")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, testCase := range []struct {
		name      string
		ctx       context.Context
		wantError error
	}{
		{name: "nil context"},
		{name: "canceled context", ctx: canceled, wantError: context.Canceled},
		{name: "expired context", ctx: expired, wantError: context.DeadlineExceeded},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			identity, err := credentialService.Authenticate(testCase.ctx, "root", "root")
			if identity != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) || queryCalls != 0 {
				t.Fatal("无效上下文仍执行查询或返回身份")
			}
		})
	}
}

func TestCredentialServiceWithRealBcrypt(t *testing.T) {
	// 1. 确认生产构造器确实绑定第 020 步方法；占位摘要也必须有效且固定成本为 12。
	hash, err := bcrypt.GenerateFromPassword([]byte(" root "), bcrypt.MinCost)
	if err != nil {
		t.Fatal("生成测试摘要失败")
	}
	credentialService := NewCredentialService(func(_ context.Context, username string) (*domain.LoginAccount, error) {
		if username != "root" {
			return nil, domain.ErrAccountNotFound
		}
		return &domain.LoginAccount{ID: 1, PasswordHash: string(hash), Status: "ACTIVE"}, nil
	})
	if identity, err := credentialService.Authenticate(context.Background(), "root", " root "); err != nil || identity == nil || identity.ID != 1 {
		t.Fatal("真实 bcrypt 没有通过正确原始密码")
	}
	if identity, err := credentialService.Authenticate(context.Background(), "root", "root"); identity != nil || err != domain.ErrInvalidCredentials {
		t.Fatal("密码被裁剪或凭证错误未统一")
	}
	if cost, err := bcrypt.Cost([]byte(missingAccountHash)); err != nil || cost != 12 {
		t.Fatal("固定占位摘要的成本或结构错误")
	}
	if identity, err := credentialService.Authenticate(context.Background(), "missing", "cloudnest-non-account-timing-fixture"); identity != nil || err != domain.ErrInvalidCredentials {
		t.Fatal("不存在的账号意外通过固定测试密码")
	}
}

func TestCredentialServiceConcurrentIsolation(t *testing.T) {
	// 1. 并发共用服务，每个请求取得自己的身份，服务不能保存上次查询的可变结果。
	credentialService := NewCredentialService(func(_ context.Context, username string) (*domain.LoginAccount, error) {
		userID, err := strconv.ParseUint(username, 10, 64)
		if err != nil {
			return nil, err
		}
		return &domain.LoginAccount{ID: userID, PasswordHash: "private-hash-" + username, Status: "ACTIVE"}, nil
	})
	credentialService.verifyPassword = func(hash, password string) (bool, error) { return hash == password, nil }
	type outcome struct {
		identity *domain.VerifiedAccount
		err      error
		expected uint64
	}
	outcomes := make(chan outcome, 24)
	for requestIndex := range 24 {
		go func(userID uint64) {
			username := strconv.FormatUint(userID, 10)
			identity, err := credentialService.Authenticate(context.Background(), username, "private-hash-"+username)
			outcomes <- outcome{identity: identity, err: err, expected: userID}
		}(uint64(requestIndex + 1))
	}
	for range 24 {
		result := <-outcomes
		if result.err != nil || result.identity == nil || result.identity.ID != result.expected {
			t.Fatal("并发凭证结果串号")
		}
	}
}

func TestCredentialServiceIntegration(t *testing.T) {
	// 1. 只读连接本项目现有 MySQL，验证 Store→Service→bcrypt，不创建会话。
	if os.Getenv("CLOUDNEST_CREDENTIAL_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_CREDENTIAL_TEST=1 后执行真实 root 凭证校验")
	}
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connectionPool, err := repository.OpenMySQL(ctx, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connectionPool.Close()
	store, err := repository.NewStore(connectionPool)
	if err != nil {
		t.Fatal(err)
	}
	credentialService := NewCredentialService(store.FindLoginAccountByUsername)
	identity, err := credentialService.Authenticate(ctx, "root", "root")
	if err != nil || identity == nil || identity.ID == 0 {
		t.Fatal("已有 root 凭证校验未通过")
	}
	for _, testCase := range []struct{ username, password string }{
		{username: "root", password: "private-wrong-password"},
		{username: "missing_" + strings.ReplaceAll(uuid.NewString(), "-", ""), password: "root"},
	} {
		if identity, err := credentialService.Authenticate(ctx, testCase.username, testCase.password); identity != nil || err != domain.ErrInvalidCredentials {
			t.Fatal("错密码和不存在账号没有返回相同凭证错误")
		}
	}
	if err := connectionPool.Close(); err != nil {
		t.Fatal(err)
	}
	if identity, err := credentialService.Authenticate(ctx, "root", "root"); identity != nil || err == nil || errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatal("数据库故障被伪装成凭证错误")
	}
}
