package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterCreateUploadRoute(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}
	authorizationCalls, createCalls, queryCalls, listCalls := 0, 0, 0, 0
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authorizationCalls++
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterDirectoryPageRoute(router, authorize, func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
		listCalls++
		return directoryPageSuccessFixture(), nil
	})
	RegisterUploadTaskRoute(router, authorize, func(ctx context.Context, current domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		queryCalls++
		if current != actor || requestcontext.RequestID(ctx) == "" || taskID != 42 {
			t.Error("任务查询范围或标识错误")
		}
		task := uploadTaskSuccessFixture()
		task.ID = taskID
		return task, nil
	})
	RegisterCreateUploadRoute(router, authorize, func(ctx context.Context, current domain.Actor, input domain.CreateUploadInput, key string) (json.RawMessage, error) {
		createCalls++
		stored, exists := requestcontext.Actor(ctx)
		if !exists || stored != actor || current != actor || requestcontext.RequestID(ctx) == "" || input.ParentID != 11 || input.Name != "学习资料 🌱" || input.SizeBytes != 40 || input.ExpectedSHA256 != nil || key != "Request-007" {
			t.Error("正式创建路由传递的身份或上传声明错误")
		}
		return json.Marshal(createUploadSuccessFixture(t))
	})
	for _, fixture := range []struct {
		name, method, path                                      string
		bearer, invalidBearer, missingKey, preflight, forbidden bool
		status, auth, creates, queries, lists                   int
	}{
		{name: "create", method: "POST", path: "/api/v1/uploads", bearer: true, status: 201, auth: 1, creates: 1},
		{name: "missing key", method: "POST", path: "/api/v1/uploads", bearer: true, missingKey: true, status: 400, auth: 1},
		{name: "authentication first", method: "POST", path: "/api/v1/uploads", missingKey: true, status: 401},
		{name: "invalid bearer", method: "POST", path: "/api/v1/uploads", invalidBearer: true, status: 401, auth: 1},
		{name: "forbidden origin", method: "POST", path: "/api/v1/uploads", bearer: true, forbidden: true, status: 403},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/uploads", preflight: true, status: 204},
		{name: "task query coexistence", method: "GET", path: "/api/v1/uploads/42", bearer: true, status: 200, auth: 1, queries: 1},
		{name: "list coexistence", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, lists: 1},
		{name: "get root", method: "GET", path: "/api/v1/uploads", status: 404},
		{name: "put root", method: "PUT", path: "/api/v1/uploads", status: 404},
		{name: "head root", method: "HEAD", path: "/api/v1/uploads", status: 404},
		{name: "trailing slash", method: "POST", path: "/api/v1/uploads/", status: 404},
		{name: "extra segment", method: "POST", path: "/api/v1/uploads/42", status: 404},
		{name: "query trailing slash", method: "GET", path: "/api/v1/uploads/42/", status: 404},
		{name: "public ping", method: "GET", path: "/ping", status: 200},
		{name: "public live", method: "GET", path: "/health/live", status: 200},
		{name: "public ready", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			authorizationCalls, createCalls, queryCalls, listCalls = 0, 0, 0, 0
			body := `{"parent_id":"11","name":"学习资料 🌱","size_bytes":40}`
			if fixture.missingKey {
				body = `{"private-broken":`
			}
			request := httptest.NewRequest(fixture.method, fixture.path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if fixture.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if fixture.invalidBearer {
				request.Header.Set("Authorization", "Bearer private-invalid-access")
			}
			if !fixture.missingKey {
				request.Header.Set("Idempotency-Key", "Request-007")
			}
			if fixture.forbidden {
				request.Header.Set("Origin", "http://private-origin.example")
			}
			if fixture.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type,Idempotency-Key")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != fixture.status || authorizationCalls != fixture.auth || createCalls != fixture.creates || queryCalls != fixture.queries || listCalls != fixture.lists {
				t.Fatal("正式上传路由、认证顺序或调用次数错误", response.Code, authorizationCalls, createCalls, queryCalls, listCalls)
			}
			if fixture.creates == 1 {
				var payload dto.CreateUploadResponse
				if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Data.Task.ID != "42" || payload.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式上传创建成功响应错误")
				}
			}
		})
	}
}

func TestRegisterCreateUploadRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "create"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成上传路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			create := func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "create" {
				create = nil
			}
			RegisterCreateUploadRoute(router, authorize, create)
		})
	}
}
