package security

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestLoginAttemptGateSettlement(t *testing.T) {
	queryFailure := errors.New("database unavailable")
	for _, testCase := range []struct {
		name                       string
		identity                   *domain.VerifiedAccount
		credentialError, wantError error
		cancelDuring               bool
		wantFailed                 bool
	}{
		{name: "success", identity: &domain.VerifiedAccount{ID: 1}},
		{name: "wrong credentials", credentialError: domain.ErrInvalidCredentials, wantError: domain.ErrInvalidCredentials, wantFailed: true},
		{name: "disabled", credentialError: domain.ErrAccountDisabled, wantError: domain.ErrAccountDisabled},
		{name: "query failure", credentialError: queryFailure, wantError: queryFailure},
		{name: "hash failure", credentialError: domain.ErrInvalidPasswordHash, wantError: domain.ErrInvalidPasswordHash},
		{name: "empty successful result", wantError: domain.ErrInvalidAccountData},
		{name: "zero id", identity: &domain.VerifiedAccount{}, wantError: domain.ErrInvalidAccountData},
		{name: "partial identity on error", identity: &domain.VerifiedAccount{ID: 99}, credentialError: queryFailure, wantError: queryFailure},
		{name: "canceled success", identity: &domain.VerifiedAccount{ID: 1}, cancelDuring: true, wantError: context.Canceled},
		{name: "canceled wrong credentials", credentialError: domain.ErrInvalidCredentials, cancelDuring: true, wantError: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var order []string
			var completed, failed bool
			gate := NewLoginAttemptGate(func(address netip.Addr, username string) (func(bool), time.Duration, error) {
				order = append(order, "begin")
				if address.String() != "192.0.2.1" || username != "root" {
					t.Error("准入没有使用实际连接 IP 和用户名")
				}
				var once sync.Once
				return func(value bool) {
					once.Do(func() { completed, failed = true, value; order = append(order, "complete") })
				}, 0, nil
			}, func(received context.Context, username, password string) (*domain.VerifiedAccount, error) {
				order = append(order, "authenticate")
				if received != ctx || username != "root" || password != " private-password " {
					t.Error("凭证输入或上下文被修改")
				}
				if testCase.cancelDuring {
					cancel()
				}
				return testCase.identity, testCase.credentialError
			})
			request := httptest.NewRequest("POST", "/test", nil).WithContext(ctx)
			request.RemoteAddr = "192.0.2.1:1234"
			request.Header.Set("X-Forwarded-For", "198.51.100.1")
			identity, retry, err := gate.Authenticate(request, "root", " private-password ")
			if retry != 0 || !completed || failed != testCase.wantFailed || strings.Join(order, ",") != "begin,authenticate,complete" {
				t.Fatalf("结算或顺序错误 order=%v failed=%v retry=%s", order, failed, retry)
			}
			if testCase.wantError != nil {
				if identity != nil || !errors.Is(err, testCase.wantError) {
					t.Fatal("失败未保留错误或返回了部分身份")
				}
			} else if err != nil || identity == nil || identity.ID != 1 {
				t.Fatal("成功身份丢失")
			}
		})
	}
}

func TestLoginAttemptGateEarlyExits(t *testing.T) {
	beginFailure := errors.New("invalid limit key")
	for _, testCase := range []struct {
		name         string
		retry        time.Duration
		beginError   error
		cancelDuring bool
	}{
		{name: "limited", retry: 1500 * time.Millisecond},
		{name: "begin failure", beginError: beginFailure},
		{name: "invalid admission result"},
		{name: "canceled after admission", cancelDuring: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			released := false
			gate := NewLoginAttemptGate(func(netip.Addr, string) (func(bool), time.Duration, error) {
				if testCase.cancelDuring {
					cancel()
					return func(bool) { released = true }, 0, nil
				}
				return nil, testCase.retry, testCase.beginError
			}, func(context.Context, string, string) (*domain.VerifiedAccount, error) {
				t.Error("提前结束后仍调用凭证服务")
				return nil, nil
			})
			identity, retry, err := gate.Authenticate(httptest.NewRequest("POST", "/test", nil).WithContext(ctx), "root", "root")
			if identity != nil {
				t.Fatal("提前退出返回了身份")
			}
			if testCase.retry > 0 {
				if retry != testCase.retry || err != nil {
					t.Fatal("等待建议丢失")
				}
			} else if retry != 0 || err == nil {
				t.Fatal("错误被当成正常结果")
			}
			if testCase.beginError != nil && !errors.Is(err, beginFailure) {
				t.Fatal("准入错误链丢失")
			}
			if testCase.cancelDuring && (!released || !errors.Is(err, context.Canceled)) {
				t.Fatal("取消未释放预留")
			}
		})
	}
	gate := NewLoginAttemptGate(func(netip.Addr, string) (func(bool), time.Duration, error) {
		t.Error("无效请求仍申请名额")
		return nil, 0, nil
	}, func(context.Context, string, string) (*domain.VerifiedAccount, error) { return nil, nil })
	if identity, retry, err := gate.Authenticate(nil, "root", "root"); identity != nil || retry != 0 || err == nil {
		t.Fatal("nil 请求未失败")
	}
	request := httptest.NewRequest("POST", "/test", nil)
	request.RemoteAddr = "private-invalid-address"
	if _, _, err := gate.Authenticate(request, "root", "root"); err == nil {
		t.Fatal("无效来源地址被接受")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := gate.Authenticate(request.WithContext(ctx), "root", "root"); !errors.Is(err, context.Canceled) {
		t.Fatal("取消请求仍申请名额")
	}
}

func TestLoginAttemptGatePanicReleasesSlot(t *testing.T) {
	// 超过十次异常仍能进入凭证方法，说明每次 panic 的兜底释放都完成。
	limiter := service.NewLoginFailureLimiter()
	gate := NewLoginAttemptGate(limiter.Begin, func(context.Context, string, string) (*domain.VerifiedAccount, error) { panic("private-test-panic") })
	for range 12 {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("名额泄漏导致异常分支没执行")
				}
			}()
			_, _, _ = gate.Authenticate(httptest.NewRequest("POST", "/test", nil), "root", "root")
		}()
	}
}
