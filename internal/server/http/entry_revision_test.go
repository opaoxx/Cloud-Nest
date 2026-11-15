package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestEntryRevisionValidationHTTP(t *testing.T) {
	// 1. 在测试入口连接认证、重命名 DTO、内存版本事实与版本校验。
	currentRevisions := map[uint64]uint64{23: 3, 24: ^uint64(0), 25: 0, 26: 9007199254740993}
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	router.PATCH("/test-entry-revision/:id", middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		if token != "fixture-access" {
			return nil, domain.ErrInvalidAccessToken
		}
		return &domain.Actor{UserID: 7, DeviceID: 13, SessionID: 17}, nil
	}, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		actor, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists || actor.UserID != 7 {
			handler.InternalError(requestContext, nil)
			return
		}
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		if _, fieldError := dto.ParseIdempotencyKey(requestContext.Request); fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.RenameEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseRenameEntryRequest(request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		current, exists := currentRevisions[path.EntryID]
		if !exists {
			requestContext.AbortWithStatusJSON(http.StatusNotFound, dto.ErrorResponse{Error: dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: "目录项不存在"}, RequestID: requestcontext.RequestID(requestContext.Request.Context())})
			return
		}
		if err := service.ValidateEntryRevision(current, input.ExpectedRevision); err != nil {
			// 包装模拟后续事务或服务错误链，提取类型信息组成公开版本冲突。
			wrapped := fmt.Errorf("private-version-check: %w", err)
			var conflict *domain.EntryRevisionConflictError
			if errors.Is(wrapped, domain.ErrEntryRevisionConflict) && errors.As(wrapped, &conflict) {
				requestContext.AbortWithStatusJSON(http.StatusConflict, dto.ErrorResponse{
					Error:     dto.ErrorDetail{Code: "REVISION_CONFLICT", Message: domain.ErrEntryRevisionConflict.Error(), Details: map[string]any{"current_revision": strconv.FormatUint(conflict.CurrentRevision, 10)}},
					RequestID: requestcontext.RequestID(requestContext.Request.Context()),
				})
				return
			}
			handler.InternalError(requestContext, nil)
			return
		}
		requestContext.AbortWithStatus(http.StatusNoContent)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	for _, testCase := range []struct {
		name, id, expected, current, field string
		status                             int
	}{
		{name: "matching", id: "23", expected: "3", status: 204},
		{name: "stale", id: "23", expected: "2", current: "3", status: 409},
		{name: "future", id: "23", expected: "4", current: "3", status: 409},
		{name: "maximum match", id: "24", expected: "18446744073709551615", status: 204},
		{name: "maximum stale", id: "24", expected: "18446744073709551614", current: "18446744073709551615", status: 409},
		{name: "javascript adjacent", id: "26", expected: "9007199254740992", current: "9007199254740993", status: 409},
		{name: "javascript matching", id: "26", expected: "9007199254740993", status: 204},
		{name: "database zero", id: "25", expected: "1", status: 500},
		{name: "missing target", id: "999", expected: "1", status: 404},
		{name: "zero input", id: "23", expected: "0", field: "expected_revision", status: 400},
		{name: "invalid path", id: "0", expected: "1", field: "id", status: 400},
		{name: "missing key", id: "23", expected: "1", field: "Idempotency-Key", status: 400},
		{name: "missing authorization", id: "0", expected: "0", status: 401},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(dto.RenameEntryRequest{Name: "private-new-name", ExpectedRevision: testCase.expected})
			if err != nil {
				t.Fatal(err)
			}
			message, err := http.NewRequest("PATCH", server.URL+"/test-entry-revision/"+testCase.id+"?note=private-query", bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			message.Header.Set("Content-Type", "application/json")
			if testCase.name != "missing authorization" {
				message.Header.Set("Authorization", "Bearer fixture-access")
			}
			if testCase.name != "missing key" {
				message.Header.Set("Idempotency-Key", "private-request-key")
			}
			response, err := client.Do(message)
			if err != nil {
				t.Fatal("版本校验实际 HTTP 请求失败", err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != testCase.status || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("版本校验状态或响应头错误", response.StatusCode, readError, closeError)
			}

			// 2. 确认错误类别、版本字符串和当前请求 ID，成功请求返回无正文。
			if testCase.status == 204 {
				if len(body) != 0 {
					t.Fatal("校验通过时测试入口存在正文")
				}
				return
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(body, &failure) != nil || failure.RequestID == "" || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("错误响应或请求关联错误")
			}
			code := "INVALID_ARGUMENT"
			switch testCase.status {
			case 409:
				code = "REVISION_CONFLICT"
				if failure.Error.Message != "文件已发生变化，请刷新后重试" || len(failure.Error.Details) != 1 || failure.Error.Details["current_revision"] != testCase.current || response.Header.Get("Pragma") != "no-cache" {
					t.Fatal("冲突提示或完整版本字符串错误")
				}
			case 500:
				code = "INTERNAL_ERROR"
			case 404:
				code = "ENTRY_NOT_FOUND"
			case 401:
				code = "AUTH_REQUIRED"
			}
			if failure.Error.Code != code {
				t.Fatal("错误类别不符")
			}
			if testCase.field != "" {
				if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != testCase.field {
					t.Fatal("字段错误定位不符")
				}
			} else if testCase.status != 409 && failure.Error.Details != nil {
				t.Fatal("普通错误 details 非空")
			}
			if bytes.Contains(body, []byte("private-")) {
				t.Fatal("版本错误响应包含原始请求或内部包装")
			}
		})
	}
	server.Close()
	for _, marker := range []string{"private-new-name", "private-query", "private-request-key", "private-version-check"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("完成日志包含请求或内部错误原文")
		}
	}
}
