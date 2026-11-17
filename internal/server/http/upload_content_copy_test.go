package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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

type uploadCopyHTTPReader struct {
	reader    io.Reader
	bytesRead int
}

func (reader *uploadCopyHTTPReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.bytesRead += count
	return count, err
}

func TestCopyUploadContentRealHTTP(t *testing.T) {
	matching := sha256.Sum256([]byte{0, 0xff, 0xfe, 1})
	wrong := sha256.Sum256([]byte("different fixture content"))
	empty := sha256.Sum256(nil)
	zero := [sha256.Size]byte{}
	for _, fixture := range []struct {
		name         string
		payload      []byte
		declared     uint64
		chunked      bool
		status       int
		copyCalled   bool
		readFailure  bool
		closedTarget bool
		expected     *[sha256.Size]byte
	}{
		{"known binary", []byte{0, 0xff, 0xfe, 1}, 4, false, 204, true, false, false, nil},
		{"chunked binary", []byte{0, 0xff, 0xfe, 1}, 4, true, 204, true, false, false, nil},
		{"chunked short", []byte{0, 0xff, 1}, 4, true, 413, true, false, false, nil},
		{"chunked long", []byte{0, 0xff, 0xfe, 1, 2, 3, 4}, 4, true, 413, true, false, false, nil},
		{"known empty", nil, 0, false, 204, true, false, false, nil},
		{"chunked empty", nil, 0, true, 204, true, false, false, nil},
		{"nonempty for zero", []byte{1}, 0, true, 413, true, false, false, nil},
		{"known mismatch before copy", []byte{1, 2, 3, 4}, 3, false, 413, false, false, false, nil},
		{"body read failure", []byte{1, 2, 3}, 3, false, 500, true, true, false, nil},
		{"closed temporary target", []byte{1, 2, 3}, 3, false, 500, true, false, true, nil},
		{"above JSON limit", bytes.Repeat([]byte{0xff}, int(binding.MaxJSONBodyBytes)+7), uint64(binding.MaxJSONBodyBytes) + 7, true, 204, true, false, false, nil},
		{"matching checksum", []byte{0, 0xff, 0xfe, 1}, 4, false, 204, true, false, false, &matching},
		{"chunked checksum mismatch", []byte{0, 0xff, 0xfe, 1}, 4, true, 422, true, false, false, &wrong},
		{"explicit zero checksum", []byte{0, 0xff, 0xfe, 1}, 4, false, 422, true, false, false, &zero},
		{"empty matching checksum", nil, 0, true, 204, true, false, false, &empty},
		{"empty checksum mismatch", nil, 0, false, 422, true, false, false, &wrong},
		{"size error before checksum", []byte{1, 2, 3}, 4, true, 413, true, false, false, &wrong},
		{"IO error before checksum", []byte{1, 2, 3}, 3, false, 500, true, true, false, &wrong},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 原始 HTTP 流先经过认证及头/长度校验，再写入调用方拥有的文件。
			checksumCalls := 0
			directory := t.TempDir()
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-copy-upload", protected, func(requestContext *gin.Context) {
				var source io.Reader = requestContext.Request.Body
				if fixture.readFailure {
					source = failedJSONReader{}
				}
				counter := &uploadCopyHTTPReader{reader: source}
				tracked := &uploadContentTrackedBody{reader: counter}
				requestContext.Request.Body = tracked
				if !binding.ValidateUploadContentHeaders(requestContext) || !binding.ValidateUploadContentLength(requestContext, fixture.declared) {
					if fixture.copyCalled || counter.bytesRead != 0 || tracked.reads != 0 || tracked.closes != 0 {
						t.Error("头或长度拒绝阶段读取了正文")
					}
					return
				}
				if !fixture.copyCalled || tracked.reads != 0 || tracked.closes != 0 {
					t.Error("进入复制阶段前的边界错误")
				}
				file, err := os.CreateTemp(directory, "content-*.part")
				if err != nil {
					t.Error(err)
					handler.InternalError(requestContext, nil)
					return
				}
				defer file.Close()
				if fixture.closedTarget {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				}
				result, copyError := service.CopyUploadContent(requestContext.Request.Context(), file, requestContext.Request.Body, fixture.declared)
				info, statError := os.Stat(file.Name())
				if statError != nil || uint64(counter.bytesRead) > fixture.declared+1 || (info != nil && uint64(info.Size()) > fixture.declared) || tracked.closes != 0 {
					t.Error("真实流读取预算、磁盘写入量或所有权错误", statError)
				}
				// 2. 测试交付层按领域错误映射 API 契约，内部 I/O 原因保持私有。
				if copyError != nil {
					if result != nil {
						t.Error("流复制失败仍交付摘要")
					}
					if errors.Is(copyError, domain.ErrUploadBodySizeMismatch) {
						requestContext.Header("Cache-Control", "no-store")
						requestContext.Header("Pragma", "no-cache")
						requestContext.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, dto.ErrorResponse{
							Error:     dto.ErrorDetail{Code: "BODY_SIZE_MISMATCH", Message: domain.ErrUploadBodySizeMismatch.Error(), Details: nil},
							RequestID: requestcontext.RequestID(requestContext.Request.Context()),
						})
						return
					}
					handler.InternalError(requestContext, nil)
					return
				}
				// 3. 完整复制成功后比较任务声明的摘要，错误按公开契约交付。
				checksumCalls++
				if err := service.ValidateUploadContentChecksum(result.SHA256, fixture.expected); err != nil {
					if !errors.Is(err, domain.ErrUploadChecksumMismatch) {
						t.Error("未知摘要校验错误", err)
						handler.InternalError(requestContext, nil)
						return
					}
					requestContext.Header("Cache-Control", "no-store")
					requestContext.Header("Pragma", "no-cache")
					requestContext.AbortWithStatusJSON(http.StatusUnprocessableEntity, dto.ErrorResponse{
						Error:     dto.ErrorDetail{Code: "CHECKSUM_MISMATCH", Message: err.Error(), Details: nil},
						RequestID: requestcontext.RequestID(requestContext.Request.Context()),
					})
					return
				}
				// 4. 同一文件描述符继续同步、定位和读取，核对磁盘内容与摘要。
				if err := file.Sync(); err != nil {
					t.Error(err)
				}
				if _, err := file.Seek(0, io.SeekStart); err != nil {
					t.Error(err)
				}
				content, err := io.ReadAll(file)
				if err != nil || result == nil || result.SizeBytes != fixture.declared || result.SHA256 != sha256.Sum256(content) || !bytes.Equal(content, fixture.payload) {
					t.Error("磁盘内容与已交付摘要不符", err)
				}
				requestContext.Status(http.StatusNoContent)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			var reader io.Reader = bytes.NewReader(fixture.payload)
			if fixture.chunked {
				reader = struct{ io.Reader }{reader}
			}
			request, err := http.NewRequest(http.MethodPut, server.URL+"/test-copy-upload?token=private-copy-query", reader)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer fixture-access")
			request.Header.Set("Content-Type", "application/octet-stream")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if response.StatusCode != fixture.status || readError != nil || closeError != nil {
				t.Fatal("实际流复制 HTTP 交付错误", response.StatusCode, readError, closeError)
			}
			wantChecksumCalls := 0
			if fixture.status == 204 || fixture.status == 422 {
				wantChecksumCalls = 1
			}
			if checksumCalls != wantChecksumCalls {
				t.Fatal("摘要比较调用顺序错误", checksumCalls)
			}
			if fixture.status != 204 {
				var result dto.ErrorResponse
				if err := json.Unmarshal(body, &result); err != nil || result.RequestID != response.Header.Get("X-Request-ID") || result.Error.Details != nil {
					t.Fatal("流复制错误结构或请求 ID 错误", err)
				}
				if response.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
					t.Fatal("错误响应头不符合契约")
				}
				if (fixture.status == 413 || fixture.status == 422) && response.Header.Get("Pragma") != "no-cache" {
					t.Fatal("上传校验错误缺少禁止缓存头")
				}
				if fixture.status == 422 && (result.Error.Code != "CHECKSUM_MISMATCH" || result.Error.Message != "文件校验失败，请重新上传") {
					t.Fatal("摘要错误码或固定提示不符")
				}
				if fixture.status == 413 && result.Error.Code != "BODY_SIZE_MISMATCH" {
					t.Fatal("大小错误码不符")
				}
				if fixture.status == 413 && fixture.copyCalled && result.Error.Message != domain.ErrUploadBodySizeMismatch.Error() {
					t.Fatal("实际大小错误提示不符")
				}
				if fixture.status == 500 && result.Error.Code != "INTERNAL_ERROR" {
					t.Fatal("内部 I/O 失败未交付通用错误")
				}
			}
			for _, private := range []string{"fixture-access", "private-copy-query", "private-reader-error", directory} {
				if strings.Contains(logs.String(), private) || strings.Contains(string(body), private) {
					t.Fatal("流复制错误或日志包含私有输入/路径")
				}
			}
		})
	}
}
