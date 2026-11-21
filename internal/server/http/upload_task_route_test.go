package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterUploadTaskRoute(t *testing.T) {
	const lostSessionTaskID = 24
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	authorizeCalls, taskCalls, listCalls := 0, 0, 0
	var receivedTarget uint64
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authorizeCalls++
		if token != "fixture-access" {
			t.Error("正式上传任务路由未准确传递 Bearer")
		}
		return &actor, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	RegisterDirectoryPageRoute(router, authorize, func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error) {
		listCalls++
		return directoryPageSuccessFixture(), nil
	})
	RegisterUploadTaskRoute(router, authorize, func(ctx context.Context, received domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		taskCalls++
		stored, exists := requestcontext.Actor(ctx)
		if received != actor || !exists || stored != actor || requestcontext.RequestID(ctx) == "" {
			t.Error("正式上传任务路由缺少准确身份或请求 ID")
		}
		receivedTarget = taskID
		result := uploadTaskSuccessFixture()
		result.ID = taskID
		if taskID == lostSessionTaskID {
			result.Status, result.UploadMode = domain.UploadStatusFailed, "MULTIPART"
			result.ReservedBytes, result.LeaseOwner, result.LeaseUntil = 0, nil, nil
			result.CompletedAt, result.LastErrorCode = &result.UpdatedAt, new(domain.UploadSessionLostCode)
			result.StorageUploadID = new("private-upload-session")
		}
		return result, nil
	})
	for _, testCase := range []struct {
		name, method, path         string
		bearer, preflight          bool
		status, auth, tasks, lists int
		targetID                   uint64
	}{
		{name: "task", method: "GET", path: "/api/v1/uploads/23", bearer: true, status: 200, auth: 1, tasks: 1, targetID: 23},
		{name: "failed multipart session", method: "GET", path: "/api/v1/uploads/24", bearer: true, status: 200, auth: 1, tasks: 1, targetID: lostSessionTaskID},
		{name: "decoded id", method: "GET", path: "/api/v1/uploads/%32%33", bearer: true, status: 200, auth: 1, tasks: 1, targetID: 23},
		{name: "full unsigned id", method: "GET", path: "/api/v1/uploads/18446744073709551615", bearer: true, status: 200, auth: 1, tasks: 1, targetID: ^uint64(0)},
		{name: "invalid id", method: "GET", path: "/api/v1/uploads/0", bearer: true, status: 400, auth: 1},
		{name: "authentication before id", method: "GET", path: "/api/v1/uploads/0", status: 401},
		{name: "preflight", method: "OPTIONS", path: "/api/v1/uploads/23", preflight: true, status: 204},
		{name: "list coexistence", method: "GET", path: "/api/v1/entries?parent_id=12", bearer: true, status: 200, auth: 1, lists: 1},
		{name: "list invalid query", method: "GET", path: "/api/v1/entries?parent_id=0", bearer: true, status: 400, auth: 1},
		{name: "ping public", method: "GET", path: "/ping", status: 200},
		{name: "live public", method: "GET", path: "/health/live", status: 200},
		{name: "ready public", method: "GET", path: "/health/ready", status: 200},
		{name: "wrong method", method: "POST", path: "/api/v1/uploads/23", status: 404},
		{name: "trailing slash", method: "GET", path: "/api/v1/uploads/23/", status: 404},
		{name: "extra segment", method: "GET", path: "/api/v1/uploads/23/download", status: 404},
		{name: "empty segment", method: "GET", path: "/api/v1/uploads/", status: 404},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authorizeCalls, taskCalls, listCalls = 0, 0, 0
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			if testCase.bearer {
				request.Header.Set("Authorization", "Bearer fixture-access")
			}
			if testCase.preflight {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
				request.Header.Set("Access-Control-Request-Method", "GET")
				request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Request-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authorizeCalls != testCase.auth || taskCalls != testCase.tasks || listCalls != testCase.lists {
				t.Fatal("正式上传任务路由匹配、认证或调用次数错误", response.Code, authorizeCalls, taskCalls, listCalls)
			}
			if testCase.tasks == 1 {
				var body dto.UploadTaskResponse
				if receivedTarget != testCase.targetID || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.ID != strconv.FormatUint(testCase.targetID, 10) ||
					body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("正式上传任务路由目标或完整响应错误")
				}
				if receivedTarget == lostSessionTaskID {
					// 核对当前任务的失败原因和面向客户端的固定提示。
					if body.Data.Status != "FAILED" || body.Data.UploadMode != "MULTIPART" || body.Data.CompletedAt == nil ||
						body.Data.ResultEntryID != nil || body.Data.LastError == nil || body.Data.LastError.Code != "UPLOAD_SESSION_LOST" ||
						body.Data.LastError.Message != "存储上传会话已失效，请重新创建任务" || strings.Contains(response.Body.String(), "private-upload") {
						t.Fatal("会话丢失任务的公开响应不符", body.Data.LastError)
					}
				}
			}
		})
	}
}

func TestRegisterUploadTaskRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "task"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍完成上传任务路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			query := func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) { return nil, nil }
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "task" {
				query = nil
			}
			RegisterUploadTaskRoute(router, authorize, query)
		})
	}
}
