package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type loginSessionTxStub struct {
	ensure func(context.Context, string, string, string) (*domain.LoginDevice, error)
	create func(context.Context, uint64, [sha256.Size]byte, time.Time) (*domain.LoginSession, error)
}

func (transaction *loginSessionTxStub) EnsureLoginDevice(ctx context.Context, installationID, name, platform string) (*domain.LoginDevice, error) {
	return transaction.ensure(ctx, installationID, name, platform)
}

func (transaction *loginSessionTxStub) CreateLoginSession(ctx context.Context, deviceID uint64, hash [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
	return transaction.create(ctx, deviceID, hash, expiresAt)
}

func TestLoginSessionServiceCommitAndCopies(t *testing.T) {
	// 1. 用独立事务上下文与真实随机凭证验证完整调用顺序和传参。
	ctx := context.Background()
	type transactionKey struct{}
	txContext := context.WithValue(ctx, transactionKey{}, true)
	now := time.Date(2026, 11, 11, 16, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	jwtConfig := accessTokenTestConfig()
	expectedExpiry := now.UTC().Add(jwtConfig.RefreshTokenTTL).Truncate(time.Microsecond)
	const installationID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	device := &domain.LoginDevice{ID: math.MaxUint64, UserID: math.MaxUint64, InstallationID: installationID, Name: "用户改过的名称", Platform: "WEB"}
	session := &domain.LoginSession{ID: math.MaxUint64, UserID: device.UserID, DeviceID: device.ID, ExpiresAt: expectedExpiry}
	var steps []string
	var storedHash [sha256.Size]byte
	transaction := &loginSessionTxStub{
		ensure: func(ctx context.Context, installation, name, platform string) (*domain.LoginDevice, error) {
			steps = append(steps, "device")
			if ctx != txContext || installation != installationID || name != "请求名称" || platform != "ANDROID" {
				t.Error("设备准备未收到事务上下文及原输入")
			}
			return device, nil
		},
		create: func(ctx context.Context, deviceID uint64, hash [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
			steps = append(steps, "session")
			if ctx != txContext || deviceID != device.ID || !expiresAt.Equal(expectedExpiry) || expiresAt.Location() != time.UTC {
				t.Error("会话创建未收到正确事务参数和 UTC 微秒期限")
			}
			storedHash = hash
			return session, nil
		},
	}
	creator, err := NewLoginSessionService(jwtConfig, func(received context.Context, userID uint64, action func(context.Context, LoginSessionTransaction) error) error {
		steps = append(steps, "begin")
		if received != ctx || userID != device.UserID {
			t.Error("用户事务未收到已验证账号与原上下文")
		}
		if err := action(txContext, transaction); err != nil {
			return err
		}
		steps = append(steps, "commit")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	creator.now = func() time.Time { return now }
	generate := creator.generateRefreshToken
	creator.generateRefreshToken = func() (string, [sha256.Size]byte) {
		steps = append(steps, "token")
		return generate()
	}
	jwtConfig.RefreshTokenTTL = time.Hour
	result, err := creator.Create(ctx, domain.VerifiedAccount{ID: device.UserID}, installationID, "请求名称", "ANDROID")
	if err != nil || result == nil || result.Device != *device || result.Session != *session ||
		len(result.RefreshToken) != 43 || sha256.Sum256([]byte(result.RefreshToken)) != storedHash {
		t.Fatal("提交结果、配置快照或刷新凭证摘要错误")
	}
	if !reflect.DeepEqual(steps, []string{"token", "begin", "device", "session", "commit"}) {
		t.Fatal("设备、会话与提交的执行顺序错误")
	}

	// 2. 返回副本保存提交时信息，普通 JSON 序列化隐藏刷新原文。
	device.Name = "随后修改"
	session.ID = 1
	if result.Device.Name != "用户改过的名称" || result.Session.ID != math.MaxUint64 {
		t.Fatal("结果持有了依赖返回对象的可变引用")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), result.RefreshToken) || strings.Contains(string(encoded), "RefreshToken") {
		t.Fatal("内部结果意外序列化了刷新原文")
	}
}

func TestLoginSessionServiceFailures(t *testing.T) {
	fault := errors.New("fixture transaction failure")
	for _, testCase := range []struct {
		stage                     string
		deviceCalls, sessionCalls int
	}{
		{stage: "begin"},
		{stage: "device error", deviceCalls: 1},
		{stage: "nil device", deviceCalls: 1},
		{stage: "zero device", deviceCalls: 1},
		{stage: "wrong device owner", deviceCalls: 1},
		{stage: "wrong installation", deviceCalls: 1},
		{stage: "revoked device", deviceCalls: 1},
		{stage: "session error", deviceCalls: 1, sessionCalls: 1},
		{stage: "nil session", deviceCalls: 1, sessionCalls: 1},
		{stage: "zero session", deviceCalls: 1, sessionCalls: 1},
		{stage: "wrong session owner", deviceCalls: 1, sessionCalls: 1},
		{stage: "wrong session device", deviceCalls: 1, sessionCalls: 1},
		{stage: "wrong expiry", deviceCalls: 1, sessionCalls: 1},
		{stage: "commit", deviceCalls: 1, sessionCalls: 1},
		{stage: "no callback"},
		{stage: "nil tx context"},
		{stage: "nil tx store"},
		{stage: "cancel before callback"},
		{stage: "cancel after device", deviceCalls: 1},
		{stage: "cancel after session", deviceCalls: 1, sessionCalls: 1},
	} {
		stage := testCase.stage
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deviceCalls, sessionCalls, transactionCalls := 0, 0, 0
			transaction := &loginSessionTxStub{
				ensure: func(context.Context, string, string, string) (*domain.LoginDevice, error) {
					deviceCalls++
					device := &domain.LoginDevice{ID: 13, UserID: 7, InstallationID: "installation", Name: "fixture", Platform: "WEB"}
					switch stage {
					case "device error":
						return nil, fault
					case "nil device":
						return nil, nil
					case "zero device":
						device.ID = 0
					case "wrong device owner":
						device.UserID = 99
					case "wrong installation":
						device.InstallationID = "another-installation"
					case "revoked device":
						revoked := time.Now()
						device.RevokedAt = &revoked
					case "cancel after device":
						cancel()
					}
					return device, nil
				},
				create: func(_ context.Context, _ uint64, _ [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
					sessionCalls++
					session := &domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: expiresAt}
					switch stage {
					case "session error":
						return nil, fault
					case "nil session":
						return nil, nil
					case "zero session":
						session.ID = 0
					case "wrong session owner":
						session.UserID = 99
					case "wrong session device":
						session.DeviceID = 99
					case "wrong expiry":
						session.ExpiresAt = expiresAt.Add(time.Second)
					case "cancel after session":
						cancel()
					}
					return session, nil
				},
			}
			creator, err := NewLoginSessionService(accessTokenTestConfig(), func(ctx context.Context, _ uint64, action func(context.Context, LoginSessionTransaction) error) error {
				transactionCalls++
				switch stage {
				case "begin":
					return fault
				case "no callback":
					return nil
				case "nil tx context":
					return action(nil, transaction)
				case "nil tx store":
					return action(ctx, nil)
				case "cancel before callback":
					cancel()
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				if stage == "commit" {
					return fault
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
			if err == nil || result != nil || transactionCalls != 1 {
				t.Fatal("失败应保留错误、隐藏全部结果并只尝试一次事务")
			}
			if strings.HasPrefix(stage, "cancel") && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因未被保留")
			}
			if (stage == "begin" || stage == "device error" || stage == "session error" || stage == "commit") && !errors.Is(err, fault) {
				t.Fatal("事务或依赖故障原因未被保留")
			}
			if deviceCalls != testCase.deviceCalls || sessionCalls != testCase.sessionCalls {
				t.Fatal("失败后执行了超出预期阶段的操作")
			}
		})
	}
}

func TestLoginSessionServiceInputs(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		ctx     context.Context
		account domain.VerifiedAccount
	}{
		{name: "nil context", account: domain.VerifiedAccount{ID: 7}},
		{name: "zero account", ctx: context.Background()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			creator, err := NewLoginSessionService(accessTokenTestConfig(), func(context.Context, uint64, func(context.Context, LoginSessionTransaction) error) error {
				t.Error("错误输入进入了事务")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			creator.generateRefreshToken = func() (string, [sha256.Size]byte) {
				t.Error("错误输入生成了凭证")
				return "", [sha256.Size]byte{}
			}
			if result, err := creator.Create(testCase.ctx, testCase.account, "installation", "fixture", "WEB"); err == nil || result != nil {
				t.Fatal("错误输入未被拒绝")
			}
		})
	}
	t.Run("canceled request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		creator, err := NewLoginSessionService(accessTokenTestConfig(), func(context.Context, uint64, func(context.Context, LoginSessionTransaction) error) error {
			t.Error("取消请求进入了事务")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB"); !errors.Is(err, context.Canceled) || result != nil {
			t.Fatal("取消请求应返回原始取消原因")
		}
	})
	for _, invalid := range []string{"config", "dependency", "date range"} {
		t.Run(invalid, func(t *testing.T) {
			cfg := accessTokenTestConfig()
			runner := func(context.Context, uint64, func(context.Context, LoginSessionTransaction) error) error {
				t.Error("配置错误或日期越界进入了事务")
				return nil
			}
			if invalid == "config" {
				cfg.RefreshTokenTTL = 0
			}
			if invalid == "dependency" {
				runner = nil
			}
			creator, err := NewLoginSessionService(cfg, runner)
			if invalid != "date range" {
				if err == nil || creator != nil {
					t.Fatal("构造器未拒绝无效配置或依赖")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			creator.now = func() time.Time { return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC) }
			if result, err := creator.Create(context.Background(), domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB"); !errors.Is(err, domain.ErrInvalidSessionData) || result != nil {
				t.Fatal("数据库日期范围越界未被提前拒绝")
			}
		})
	}
}

func TestLoginSessionServiceConfirmedCommit(t *testing.T) {
	// 提交确认后到来的取消信号保留在 ctx，服务仍返回已经提交的结果。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transaction := &loginSessionTxStub{
		ensure: func(context.Context, string, string, string) (*domain.LoginDevice, error) {
			return &domain.LoginDevice{ID: 13, UserID: 7, InstallationID: "installation"}, nil
		},
		create: func(_ context.Context, _ uint64, _ [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
			return &domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: expiresAt}, nil
		},
	}
	creator, err := NewLoginSessionService(accessTokenTestConfig(), func(ctx context.Context, _ uint64, action func(context.Context, LoginSessionTransaction) error) error {
		if err := action(ctx, transaction); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
	if err != nil || result == nil || result.RefreshToken == "" || ctx.Err() != context.Canceled {
		t.Fatal("提交确认后的结果或取消状态丢失")
	}
}

func TestLoginSessionServiceDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	transaction := &loginSessionTxStub{
		ensure: func(ctx context.Context, _ string, _ string, _ string) (*domain.LoginDevice, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		create: func(context.Context, uint64, [sha256.Size]byte, time.Time) (*domain.LoginSession, error) {
			t.Error("请求到期后仍创建了会话")
			return nil, nil
		},
	}
	creator, err := NewLoginSessionService(accessTokenTestConfig(), func(ctx context.Context, _ uint64, action func(context.Context, LoginSessionTransaction) error) error {
		return action(ctx, transaction)
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
	if !errors.Is(err, context.DeadlineExceeded) || result != nil {
		t.Fatal("事务内超时未保留原因或返回了部分结果")
	}
}
