package http

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterUploadCancellationRoute(t *testing.T) {
	// 1. 装配查询与取消路由，记录授权和处理调用。
	const origin = "http://127.0.0.1:5173"
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 11}
	var authorizeCalls, cancelCalls, queryCalls int
	var targetID uint64
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authorizeCalls++
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
	RegisterUploadTaskRoute(router, authorize, func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
		queryCalls++
		return uploadTaskSuccessFixture(), nil
	})
	RegisterUploadCancellationRoute(router, authorize, func(ctx context.Context, received domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		cancelCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" {
			t.Error("正式取消路由缺少准确身份或请求 ID")
		}
		targetID = taskID
		result := uploadTaskSuccessFixture()
		result.ID, result.Status = taskID, domain.UploadStatusCancelling
		return result, nil
	})

	// 2. 通过公共路由核对匹配、授权先后和跨域预检。
	for _, scenario := range []struct {
		name, method, path, token      string
		status, auth, cancels, queries int
		preflight                      bool
		taskID                         uint64
	}{
		{name: "cancel", method: "POST", path: "/api/v1/uploads/19/cancel", token: "fixture-access", status: 202, auth: 1, cancels: 1, taskID: 19},
		{name: "decoded ID", method: "POST", path: "/api/v1/uploads/%31%39/cancel", token: "fixture-access", status: 202, auth: 1, cancels: 1, taskID: 19},
		{name: "maximum ID", method: "POST", path: "/api/v1/uploads/18446744073709551615/cancel", token: "fixture-access", status: 202, auth: 1, cancels: 1, taskID: math.MaxUint64},
		{name: "authorization before path", method: "POST", path: "/api/v1/uploads/0/cancel", status: 401},
		{name: "invalid bearer", method: "POST", path: "/api/v1/uploads/19/cancel", token: "invalid", status: 401, auth: 1},
		{name: "invalid ID", method: "POST", path: "/api/v1/uploads/0/cancel", token: "fixture-access", status: 400, auth: 1},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/uploads/19/cancel", status: 204, preflight: true},
		{name: "task query", method: "GET", path: "/api/v1/uploads/19", token: "fixture-access", status: 200, auth: 1, queries: 1},
		{name: "only POST", method: "GET", path: "/api/v1/uploads/19/cancel", status: 404},
		{name: "extra slash", method: "POST", path: "/api/v1/uploads/19/cancel/", status: 404},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			authorizeCalls, cancelCalls, queryCalls = 0, 0, 0
			request := httptest.NewRequest(scenario.method, scenario.path, nil)
			if scenario.token != "" {
				request.Header.Set("Authorization", "Bearer "+scenario.token)
			}
			if scenario.preflight {
				request.Header.Set("Origin", origin)
				request.Header.Set("Access-Control-Request-Method", http.MethodPost)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != scenario.status || authorizeCalls != scenario.auth || cancelCalls != scenario.cancels || queryCalls != scenario.queries {
				t.Fatal("正式取消路由匹配、认证或调用次数不符", response.Code, authorizeCalls, cancelCalls, queryCalls)
			}
			if scenario.preflight && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("取消预检缺少允许来源")
			}
			if cancelCalls == 1 {
				var body dto.UploadTaskResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || targetID != scenario.taskID || body.Data.ID != strconv.FormatUint(scenario.taskID, 10) ||
					body.Data.Status != "CANCELLING" || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("正式取消路由任务结果不符", body, targetID)
				}
			}
		})
	}
}

func TestRegisterUploadCancellationRouteDependencies(t *testing.T) {
	// 1. 分别移除路由器、授权方法或受理方法，装配须及时失败。
	for _, missing := range []string{"router", "authorization", "cancellation"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成取消路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			begin := func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "cancellation" {
				begin = nil
			}
			RegisterUploadCancellationRoute(router, authorize, begin)
		})
	}
}
