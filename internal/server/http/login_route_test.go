package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestRegisterLoginRoute(t *testing.T) {
	result, profile := loginSuccessFixture()
	calls := 0
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	authenticate := func(*http.Request, string, string) (*domain.VerifiedAccount, time.Duration, error) {
		calls++
		return &domain.VerifiedAccount{ID: 7}, 0, nil
	}
	create := func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginTokenResult, error) {
		return result, nil
	}
	getProfile := func(context.Context, uint64) (*domain.UserProfile, error) { return profile, nil }
	RegisterLoginRoute(router, []string{"http://127.0.0.1:5173"}, authenticate, create, getProfile, false)
	request := loginHandlerRequest(t, "/api/v1/auth/login", "native", "root")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 || calls != 1 {
		t.Fatal("正式登录路径未连接处理函数")
	}
	for _, path := range []string{"/ping", "/health/live", "/health/ready"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || calls != 1 {
			t.Fatal("公共入口受到了登录认证影响")
		}
	}
	wrongMethod := httptest.NewRecorder()
	router.ServeHTTP(wrongMethod, httptest.NewRequest("GET", "/api/v1/auth/login", nil))
	if wrongMethod.Code != 404 || calls != 1 {
		t.Fatal("登录路径的错误方法触发了凭证校验")
	}
}
