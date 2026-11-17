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

func TestWithUploadContentFileRealHTTP(t *testing.T) {
	capacity := uint64(128 * 1024 * 1024)
	matching := sha256.Sum256([]byte{0, 0xff, 0xfe, 1})
	wrong := [sha256.Size]byte{}
	for _, fixture := range []struct {
		name        string
		payload     []byte
		declared    uint64
		chunked     bool
		expected    *[sha256.Size]byte
		readFailure bool
		useFailure  bool
		status      int
		calls       int
		occupied    uint64
	}{
		{"known binary", []byte{0, 0xff, 0xfe, 1}, 4, false, &matching, false, false, 204, 1, 0},
		{"chunked binary", []byte{0, 0xff, 0xfe, 1}, 4, true, nil, false, false, 204, 1, 0},
		{"empty", nil, 0, true, nil, false, false, 204, 1, 0},
		{"chunked short", []byte{0, 0xff, 1}, 4, true, nil, false, false, 413, 0, 0},
		{"chunked long", []byte{0, 0xff, 0xfe, 1, 2}, 4, true, nil, false, false, 413, 0, 0},
		{"known length mismatch", []byte{0, 0xff, 1}, 4, false, nil, false, false, 413, 0, 0},
		{"checksum mismatch", []byte{0, 0xff, 0xfe, 1}, 4, true, &wrong, false, false, 422, 0, 0},
		{"source failure", []byte{1}, 1, false, nil, true, false, 500, 0, 0},
		{"consumer failure", []byte{0, 0xff, 0xfe, 1}, 4, false, nil, false, true, 500, 1, 0},
		{"above JSON limit", bytes.Repeat([]byte{0xff}, int(binding.MaxJSONBodyBytes)+7), uint64(binding.MaxJSONBodyBytes) + 7, true, nil, false, false, 204, 1, 0},
		{"full budget", []byte{1}, 1, true, nil, false, false, 507, 0, capacity},
		{"exact budget remainder", []byte{0, 0xff, 0xfe, 1}, 4, false, nil, false, false, 204, 1, capacity - 4},
		{"zero with full budget", nil, 0, true, nil, false, false, 204, 1, capacity},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			space := service.NewUploadTempSpace()
			occupiedRelease, reserveError := space.Reserve(context.Background(), fixture.occupied)
			if reserveError != nil {
				t.Fatal(reserveError)
			}
			defer occupiedRelease()
			directory := t.TempDir()
			receiver, err := service.NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			calls := 0
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
				if token != "fixture-access" {
					return nil, domain.ErrInvalidAccessToken
				}
				return &domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, nil
			}, handler.AuthorizationFailure)
			router.PUT("/test-upload-file", protected, func(requestContext *gin.Context) {
				// 1. 保留正式输入校验顺序，然后交给临时文件作用域接收。
				if !binding.ValidateUploadContentHeaders(requestContext) || !binding.ValidateUploadContentLength(requestContext, fixture.declared) {
					return
				}
				var source io.Reader = requestContext.Request.Body
				if fixture.readFailure {
					source = failedJSONReader{}
				}
				tracked := &uploadContentTrackedBody{reader: source}
				err := receiver.WithFile(requestContext.Request.Context(), tracked, fixture.declared, fixture.expected, func(content io.ReadSeeker, digest domain.UploadContentDigest) error {
					calls++
					payload, err := io.ReadAll(content)
					if err != nil || !bytes.Equal(payload, fixture.payload) || digest.SizeBytes != fixture.declared || digest.SHA256 != sha256.Sum256(payload) {
						t.Error("HTTP 文件回调内容或摘要错误", err)
					}
					if fixture.useFailure {
						return errors.New("private-consumer-failure")
					}
					return nil
				})
				// 2. 返回前临时文件已经回收，来源仍交由请求所有者管理。
				entries, readError := os.ReadDir(directory)
				if readError != nil || len(entries) != 0 || tracked.closes != 0 {
					t.Error("HTTP 临时文件清理或正文所有权错误", readError)
				}
				if errors.Is(err, domain.ErrUploadTempSpaceExhausted) && (tracked.reads != 0 || calls != 0) {
					t.Error("容量拒绝仍接收了文件")
				}
				probe, probeError := space.Reserve(context.Background(), capacity-fixture.occupied)
				if probeError != nil || probe == nil {
					t.Error("清理后本次预算无法复用", probeError)
				} else {
					probe()
				}
				if err == nil {
					requestContext.Status(http.StatusNoContent)
					return
				}
				// 3. 测试交付层复用既有错误契约，内部路径与原因保持私有。
				status, code, message := http.StatusRequestEntityTooLarge, "BODY_SIZE_MISMATCH", domain.ErrUploadBodySizeMismatch.Error()
				switch {
				case errors.Is(err, domain.ErrUploadTempSpaceExhausted):
					status, code, message = http.StatusInsufficientStorage, "TEMP_SPACE_EXHAUSTED", domain.ErrUploadTempSpaceExhausted.Error()
				case errors.Is(err, domain.ErrUploadChecksumMismatch):
					status, code, message = http.StatusUnprocessableEntity, "CHECKSUM_MISMATCH", domain.ErrUploadChecksumMismatch.Error()
				case errors.Is(err, domain.ErrUploadBodySizeMismatch):
				default:
					handler.InternalError(requestContext, nil)
					return
				}
				requestContext.Header("Cache-Control", "no-store")
				requestContext.Header("Pragma", "no-cache")
				requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
					Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
					RequestID: requestcontext.RequestID(requestContext.Request.Context()),
				})
			})
			server := httptest.NewServer(router)
			defer server.Close()
			var source io.Reader = bytes.NewReader(fixture.payload)
			if fixture.chunked {
				source = struct{ io.Reader }{source}
			}
			request, err := http.NewRequest(http.MethodPut, server.URL+"/test-upload-file?token=private-query", source)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("Authorization", "Bearer fixture-access")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			payload, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || response.StatusCode != fixture.status || calls != fixture.calls {
				t.Fatal("临时文件 HTTP 结果或回调次数错误", response.StatusCode, calls, readError, closeError)
			}
			if fixture.status != 204 {
				var result dto.ErrorResponse
				if err := json.Unmarshal(payload, &result); err != nil || result.RequestID == "" || result.RequestID != response.Header.Get("X-Request-ID") || result.Error.Details != nil || response.Header.Get("Cache-Control") != "no-store" {
					t.Fatal("错误响应结构、缓存或请求 ID 错误", err)
				}
				want := map[int]string{413: "BODY_SIZE_MISMATCH", 422: "CHECKSUM_MISMATCH", 500: "INTERNAL_ERROR", 507: "TEMP_SPACE_EXHAUSTED"}
				if fixture.status == 507 && (result.Error.Message != "上传临时空间不足，请稍后重试" || response.Header.Get("Pragma") != "no-cache") {
					t.Fatal("容量拒绝提示或缓存契约错误")
				}
				if result.Error.Code != want[fixture.status] {
					t.Fatal("错误分类不符", result.Error.Code)
				}
			}
			for _, private := range []string{directory, "fixture-access", "private-query", "private-reader-error", "private-consumer-failure"} {
				if strings.Contains(logs.String(), private) || strings.Contains(string(payload), private) {
					t.Fatal("错误或日志泄漏私有路径与输入")
				}
			}
		})
	}
}
