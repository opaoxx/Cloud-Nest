package http

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestTrashEntryRequestHashHTTPFlow(t *testing.T) {
	// 1. 用测试入口将路径和正文解析为生成摘要的两个参数。
	const path = "/test-trash-entry-hash/"
	var acceptedHash [sha256.Size]byte
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST(path+":id", func(requestContext *gin.Context) {
		target, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		if _, fieldError := dto.ParseIdempotencyKey(requestContext.Request); fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.TrashEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseTrashEntryRequest(request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		digest, err := service.BuildTrashEntryRequestHash(target.EntryID, input.ExpectedRevision)
		if err != nil {
			t.Error("已校验的回收参数无法生成摘要", err)
			handler.InternalError(requestContext, nil)
			return
		}
		acceptedHash = digest
		requestContext.Status(http.StatusNoContent)
	})
	requestHash := func(entryID, body, key, requestID string) [sha256.Size]byte {
		t.Helper()
		acceptedHash = [sha256.Size]byte{}
		request := httptest.NewRequest(http.MethodPost, path+entryID, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("X-Request-ID", requestID)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || response.Header().Get("X-Request-ID") != requestID ||
			acceptedHash == [sha256.Size]byte{} {
			t.Fatal("测试请求未取得回收摘要", response.Code)
		}
		return acceptedHash
	}

	// 2. 改变报文写法或请求头，再与改变实际参数的结果比较。
	const basicBody = `{"expected_revision":"3"}`
	const firstRequestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const nextRequestID = "28ee758a-0727-42de-ae7d-3bb33155e149"
	original := requestHash("23", basicBody, "Request-A", firstRequestID)
	for _, scenario := range []struct {
		name, entryID, body, key string
		same                     bool
	}{
		{"JSON whitespace", "23", "{\n  \"expected_revision\" : \"3\" \n}", "Request-A", true},
		{"JSON escaped digit", "23", `{"expected_revision":"\u0033"}`, "Request-A", true},
		{"other key and request ID", "23", basicBody, "Request-B", true},
		{"other target", "24", basicBody, "Request-A", false},
		{"other revision", "23", `{"expected_revision":"4"}`, "Request-A", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if (requestHash(scenario.entryID, scenario.body, scenario.key, nextRequestID) == original) != scenario.same {
				t.Fatal("请求参数对应的摘要关系错误")
			}
		})
	}
}
