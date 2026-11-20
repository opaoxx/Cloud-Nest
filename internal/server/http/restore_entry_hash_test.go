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

func TestRestoreEntryRequestHashHTTPFlow(t *testing.T) {
	const path = "/test-restore-entry-hash/"
	var digest [sha256.Size]byte
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })

	// 1. 在测试路由中先解析路径和请求正文，再生成摘要。
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
		var request dto.RestoreEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseRestoreEntryRequest(request)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		var err error
		digest, err = service.BuildRestoreEntryRequestHash(target.EntryID, input.ExpectedRevision)
		if err != nil {
			t.Errorf("合法恢复请求无法生成摘要，错误为 %v", err)
			handler.InternalError(requestContext, nil)
			return
		}
		requestContext.Status(http.StatusNoContent)
	})
	readHash := func(entryID, body, key, requestID string) [sha256.Size]byte {
		t.Helper()
		digest = [sha256.Size]byte{}
		request := httptest.NewRequest(http.MethodPost, path+entryID, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("X-Request-ID", requestID)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || digest == [sha256.Size]byte{} {
			t.Fatalf("生成摘要的测试请求状态为 %d，正文为 %s", response.Code, response.Body.String())
		}
		return digest
	}

	// 2. 比较等价报文、独立请求头及真正不同的业务参数。
	const standardBody = `{"expected_revision":"4"}`
	const firstID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const nextID = "28ee758a-0727-42de-ae7d-3bb33155e149"
	original := readHash("23", standardBody, "restore-first", firstID)
	for _, testCase := range []struct {
		name, entryID, body, key string
		same                     bool
	}{
		{"JSON whitespace", "23", "{\n  \"expected_revision\" : \"4\"\n}", "restore-first", true},
		{"escaped digit", "23", `{"expected_revision":"\u0034"}`, "restore-first", true},
		{"different idempotency key and request ID", "23", standardBody, "restore-second", true},
		{"different target", "24", standardBody, "restore-first", false},
		{"different version", "23", `{"expected_revision":"5"}`, "restore-first", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if (readHash(testCase.entryID, testCase.body, testCase.key, nextID) == original) != testCase.same {
				t.Fatal("规范业务参数与恢复请求摘要的对应关系错误")
			}
		})
	}
}
