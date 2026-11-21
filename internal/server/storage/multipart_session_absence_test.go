package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestConfirmMultipartSessionAbsence(t *testing.T) {
	const sameInstance, otherInstance = "fixture-instance", "replacement-instance"
	const privateDetail = "private-verifier-output"
	verifierFailure := errors.New(privateDetail)
	for _, fixture := range []struct {
		name, before, after, headCode  string
		beforeErr, afterErr, wantError error
		listStatus, headStatus         int
		omitListRequestID, cancelAfter bool
		wantMissing                    bool
		wantChecks, wantReads          int32
	}{
		{name: "same verified instance", before: sameInstance, after: sameInstance, wantMissing: true, wantChecks: 2, wantReads: 2},
		{name: "no verifier"},
		{name: "endpoint not verified", after: sameInstance, wantChecks: 1},
		{name: "initial inspection failed", beforeErr: verifierFailure, wantError: domain.ErrUploadStorageUnavailable, wantChecks: 1},
		{name: "instance replaced after reads", before: sameInstance, after: otherInstance, wantChecks: 2, wantReads: 2},
		{name: "endpoint lost verification", before: sameInstance, wantChecks: 2, wantReads: 2},
		{name: "final inspection failed", before: sameInstance, afterErr: verifierFailure, wantError: domain.ErrUploadStorageUnavailable, wantChecks: 2, wantReads: 2},
		{name: "session readable", before: sameInstance, listStatus: http.StatusOK, wantChecks: 1, wantReads: 1},
		{name: "NoSuchUpload with wrong HTTP status", before: sameInstance, listStatus: http.StatusForbidden, wantError: domain.ErrUploadStorageUnavailable, wantChecks: 1, wantReads: 1},
		{name: "NoSuchUpload without request ID", before: sameInstance, omitListRequestID: true, wantError: domain.ErrUploadStorageUnavailable, wantChecks: 1, wantReads: 1},
		{name: "object exists", before: sameInstance, headStatus: http.StatusOK, wantChecks: 1, wantReads: 2},
		{name: "bucket missing", before: sameInstance, headCode: "NoSuchBucket", wantError: domain.ErrUploadStorageUnavailable, wantChecks: 1, wantReads: 2},
		{name: "canceled before confirming result", before: sameInstance, after: sameInstance, cancelAfter: true, wantError: context.Canceled, wantChecks: 2, wantReads: 2},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 模拟同一内部端点的签名 GET 和 HEAD，记录实际读取顺序。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var checks, reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && request.URL.Query().Has("location"):
					_, _ = io.WriteString(writer, "<LocationConstraint>us-east-1</LocationConstraint>")
				case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
					writer.WriteHeader(http.StatusOK)
				case request.URL.Path == "/cloudnest/"+listPartsFixtureKey:
					readNumber := reads.Add(1)
					if checks.Load() != 1 || !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
						t.Error("请求没有在同轮实例检查后使用内部客户端")
					}
					switch request.Method {
					case http.MethodGet:
						if query := request.URL.Query(); readNumber != 1 || query.Get("uploadId") != listPartsFixtureID ||
							query.Get("part-number-marker") != "0" || query.Get("max-parts") != "1" {
							t.Error("ListParts 未从原会话第一页开始")
						}
						if fixture.listStatus == http.StatusOK {
							writeUploadPartsPageFixture(t, writer, 0, 0, nil)
							return
						}
						if !fixture.omitListRequestID {
							writer.Header().Set("x-amz-request-id", "fixture-list")
						}
						status := fixture.listStatus
						if status == 0 {
							status = http.StatusNotFound
						}
						writer.WriteHeader(status)
						_, _ = io.WriteString(writer, "<Error><Code>NoSuchUpload</Code><Message>private-verifier-output</Message></Error>")
					case http.MethodHead:
						if readNumber != 2 || request.URL.RawQuery != "" {
							t.Error("HEAD 未在 ListParts 之后读取原键")
						}
						writer.Header().Set("x-amz-request-id", "fixture-head")
						status := fixture.headStatus
						if status == http.StatusOK {
							writer.Header().Set("Last-Modified", "Tue, 17 Nov 2026 08:00:00 GMT")
							writer.Header().Set("Content-Length", "16777217")
							writer.Header().Set("ETag", `"existing"`)
						} else {
							status = http.StatusNotFound
							writer.Header().Set("X-Minio-Error-Code", fixture.headCode)
						}
						writer.WriteHeader(status)
					default:
						t.Error("缺失确认发送了写请求", request.Method)
						writer.WriteHeader(http.StatusMethodNotAllowed)
					}
				default:
					t.Error("请求未使用原存储位置", request.Method)
					writer.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			client, err := NewClient(ctx, storageTestConfig(t, server.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			// 2. 夹具只为传入的内部端点返回实例身份，模拟运行实例变化。
			verify := MultipartSessionLockVerifier(func(callContext context.Context, endpoint string) (string, error) {
				if endpoint != server.URL || callContext.Err() != nil {
					t.Fatal("实例核对未绑定当前内部端点或有效上下文")
				}
				if checks.Add(1) == 1 {
					if reads.Load() != 0 {
						t.Fatal("首次核对发生在读请求之后")
					}
					return fixture.before, fixture.beforeErr
				}
				if reads.Load() != 2 {
					t.Fatal("最终核对发生在两次存储读取完成之前")
				}
				if fixture.cancelAfter {
					cancel()
				}
				return fixture.after, fixture.afterErr
			})
			if fixture.name == "no verifier" {
				verify = nil
			}
			missing, err := client.ConfirmMultipartSessionAbsence(ctx, listPartsFixtureKey, listPartsFixtureID, verify)
			if missing != fixture.wantMissing || !errors.Is(err, fixture.wantError) || (err != nil && strings.Contains(err.Error(), privateDetail)) ||
				checks.Load() != fixture.wantChecks || reads.Load() != fixture.wantReads {
				t.Fatal("会话缺失确认、读取次数或检查顺序不符", missing, err, checks.Load(), reads.Load())
			}
		})
	}
}
