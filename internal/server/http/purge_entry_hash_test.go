package http

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestPurgeEntryRequestHashHTTPFlow(t *testing.T) {
	const path = "/test-entry-purge-hash/"
	const firstID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const nextID = "28ee758a-0727-42de-ae7d-3bb33155e149"
	const previewDigest = "eyJ2IjoxfQ.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	var observed [sha256.Size]byte
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })

	// 1. 在测试入口解析路径和头部，将确认参数传给摘要方法。
	router.DELETE(path+":id", func(requestContext *gin.Context) {
		target, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		if _, fieldError := dto.ParseIdempotencyKey(requestContext.Request); fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		input, fieldError := dto.ParsePurgeEntryHeaders(requestContext.Request.Header)
		if fieldError != nil {
			handler.InvalidArgument(requestContext, fieldError)
			return
		}
		digest, err := service.BuildPurgeEntryRequestHash(target.EntryID, input.ExpectedRevision, input.PreviewDigest)
		if err != nil {
			t.Error("合法确认参数无法生成摘要", err)
			handler.InternalError(requestContext, nil)
			return
		}
		observed = digest
		requestContext.Status(http.StatusNoContent)
	})

	hash := func(revision, credential, key, requestID string) [sha256.Size]byte {
		t.Helper()
		observed = [sha256.Size]byte{}
		request := httptest.NewRequest(http.MethodDelete, path+"23", nil)
		request.Header.Set("X-Request-ID", requestID)
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set(dto.PurgeExpectedRevisionHeader, revision)
		request.Header.Set(dto.PurgePreviewDigestHeader, credential)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || observed == ([sha256.Size]byte{}) {
			t.Fatal("测试入口没有生成摘要", response.Code)
		}
		return observed
	}

	// 2. 分别改动请求头标识和确认参数，核对摘要变化。
	original := hash("4", previewDigest, "purge-first", firstID)
	if hash("4", previewDigest, "purge-second", nextID) != original {
		t.Fatal("请求 ID 或幂等键不应进入业务参数摘要")
	}
	if hash("5", previewDigest, "purge-first", nextID) == original ||
		hash("4", "f"+previewDigest[1:], "purge-first", nextID) == original {
		t.Fatal("预期版本和凭证都应进入确认请求摘要")
	}
}
