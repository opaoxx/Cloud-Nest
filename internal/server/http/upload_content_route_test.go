package http

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRegisterUploadContentRoute(t *testing.T) {
	const path = "/api/v1/uploads/19/content"
	const origin = "http://127.0.0.1:5173"
	const payload = "a\x00\xffb"
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	var authCalls, queryCalls, receiveCalls, createCalls atomic.Int32
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authCalls.Add(1)
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &actor, nil
	}
	getTask := func(ctx context.Context, current domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		queryCalls.Add(1)
		if current != actor || taskID != 19 || requestcontext.RequestID(ctx) == "" {
			t.Error("路由未传递可信身份、任务 ID 或请求 ID")
		}
		task := uploadTaskSuccessFixture()
		task.FileSize = uint64(len(payload))
		return task, nil
	}
	receive := func(ctx context.Context, current domain.Actor, task *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
		receiveCalls.Add(1)
		var data strings.Builder
		if _, err := service.CopyUploadContent(ctx, &data, source, task.FileSize); err != nil {
			return nil, err
		}
		if current != actor || task.ID != 19 || data.String() != payload {
			t.Error("上传正文、任务或身份交接错误")
		}
		result := *task
		result.Status = domain.UploadStatusVerifying
		return &result, nil
	}
	router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
	RegisterUploadTaskRoute(router, authorize, getTask)
	createSnapshot, err := json.Marshal(createUploadSuccessFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	RegisterCreateUploadRoute(router, authorize, func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error) {
		createCalls.Add(1)
		return createSnapshot, nil
	})
	RegisterUploadContentRoute(router, authorize, getTask, receive)
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 4 * time.Second

	for _, fixture := range []struct {
		name, method, path, token, origin, media, body string
		status                                         int
		calls                                          [4]int32
	}{
		{"accept binary", "PUT", path, "fixture-access", origin, "application/octet-stream", payload, 202, [4]int32{1, 1, 1, 0}},
		{"authentication first", "PUT", "/api/v1/uploads/0/content", "", "", "", "", 401, [4]int32{}},
		{"bad bearer", "PUT", path, "invalid-access", "", "", "", 401, [4]int32{1, 0, 0, 0}},
		{"path error", "PUT", "/api/v1/uploads/0/content", "fixture-access", "", "", "", 400, [4]int32{1, 0, 0, 0}},
		{"media error", "PUT", path, "fixture-access", "", "application/json", payload, 415, [4]int32{1, 0, 0, 0}},
		{"length error", "PUT", path, "fixture-access", "", "application/octet-stream", "abc", 413, [4]int32{1, 1, 0, 0}},
		{"source first", "PUT", path, "fixture-access", "http://private-origin.example", "", "", 403, [4]int32{}},
		{"preflight", "OPTIONS", path, "", origin, "", "", 204, [4]int32{}},
		{"wrong method", "GET", path, "fixture-access", "", "", "", 404, [4]int32{}},
		{"trailing slash", "PUT", path + "/", "fixture-access", "", "", "", 404, [4]int32{}},
		{"query route", "GET", "/api/v1/uploads/19", "fixture-access", "", "", "", 200, [4]int32{1, 1, 0, 0}},
		{"create route", "POST", "/api/v1/uploads", "fixture-access", "", "application/json", `{"parent_id":"11","name":"学习资料 🌱","size_bytes":40}`, 201, [4]int32{1, 0, 0, 1}},
		{"unrelated route", "GET", "/ping", "", "", "", "", 200, [4]int32{}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			before := [4]int32{authCalls.Load(), queryCalls.Load(), receiveCalls.Load(), createCalls.Load()}
			request, err := http.NewRequest(fixture.method, server.URL+fixture.path, strings.NewReader(fixture.body))
			if err != nil {
				t.Fatal(err)
			}
			if fixture.token != "" {
				request.Header.Set("Authorization", "Bearer "+fixture.token)
			}
			if fixture.origin != "" {
				request.Header.Set("Origin", fixture.origin)
			}
			if fixture.media != "" {
				request.Header.Set("Content-Type", fixture.media)
			}
			if fixture.method == http.MethodOptions {
				request.Header.Set("Access-Control-Request-Method", http.MethodPut)
				request.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
			}
			if fixture.method == http.MethodPost {
				request.Header.Set("Idempotency-Key", "route-fixture-1")
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			after := [4]int32{authCalls.Load(), queryCalls.Load(), receiveCalls.Load(), createCalls.Load()}
			for index := range after {
				after[index] -= before[index]
			}
			if response.StatusCode != fixture.status || after != fixture.calls || readError != nil || closeError != nil {
				t.Fatal("内容路由或共存路由结果错误", response.StatusCode, string(body), after, readError, closeError)
			}
			if response.StatusCode == http.StatusAccepted {
				var value dto.UploadContentAcceptedResponse
				if json.Unmarshal(body, &value) != nil || value.Data.TaskID != "19" || value.Data.Status != "VERIFYING" ||
					value.RequestID == "" || value.RequestID != response.Header.Get("X-Request-ID") ||
					response.Header.Get("Access-Control-Allow-Origin") != origin || response.Close {
					t.Fatal("正式上传内容响应错误", string(body), response.Header)
				}
			}
		})
	}
}

func TestRegisterUploadContentRouteFinishesRejectedBody(t *testing.T) {
	for _, fixture := range []struct {
		name, header, code string
		status             int
	}{
		{"source rejection", "Origin: http://private-origin.example\r\nAuthorization: Bearer fixture-access\r\n", "CORS_FORBIDDEN", 403},
		{"authentication rejection", "", "AUTH_REQUIRED", 401},
		{"media rejection", "Authorization: Bearer fixture-access\r\n", "UNSUPPORTED_MEDIA_TYPE", 415},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			RegisterUploadContentRoute(router, func(context.Context, string) (*domain.Actor, error) {
				if fixture.status != 415 {
					t.Error("来源或凭证不符时仍访问授权服务")
				}
				return &domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, nil
			}, func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
				t.Error("提前拒绝时仍读取任务")
				return nil, domain.ErrUploadTaskNotFound
			}, func(context.Context, domain.Actor, *domain.UploadTask, io.Reader) (*domain.UploadTask, error) {
				t.Error("提前拒绝时仍调用接收")
				return nil, domain.ErrUploadTaskBusy
			})
			server := httptest.NewServer(router)
			defer server.Close()
			connection := openUploadBodyTestConnection(t, server)
			defer connection.Close()
			if _, err := fmt.Fprintf(connection, "PUT /api/v1/uploads/19/content HTTP/1.1\r\nHost: localhost\r\n%sContent-Type: application/json\r\nContent-Length: 4\r\n\r\nab", fixture.header); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			response, body := readUploadBodyTestResponse(t, reader, http.MethodPut)
			var value dto.ErrorResponse
			if response.StatusCode != fixture.status || !response.Close || json.Unmarshal(body, &value) != nil ||
				value.Error.Code != fixture.code || value.RequestID != response.Header.Get("X-Request-ID") || value.RequestID == "" {
				t.Fatal("未交付原响应或未收尾连接", response.StatusCode, response.Close, string(body))
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatal("提前拒绝后未及时关闭连接", err)
			}
		})
	}
}

func TestRegisterUploadContentRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "lookup", "receive"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍注册了上传内容路由")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			getTask := func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) { return nil, nil }
			receive := func(context.Context, domain.Actor, *domain.UploadTask, io.Reader) (*domain.UploadTask, error) {
				return nil, nil
			}
			switch missing {
			case "authorization":
				authorize = nil
			case "lookup":
				getTask = nil
			case "receive":
				receive = nil
			}
			RegisterUploadContentRoute(router, authorize, getTask, receive)
		})
	}
}
