package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadContentClient 使用实际 SDK 及签名请求，将对象 PUT 交给本用例验证。
func uploadContentClient(t *testing.T, put http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Query().Has("location"):
			response.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
		case request.Method == http.MethodHead && strings.Trim(request.URL.Path, "/") == "cloudnest":
			response.WriteHeader(http.StatusOK)
		case request.Method == http.MethodPut && request.URL.RawQuery == "":
			put(response, request)
		default:
			t.Error("出现预期以外的存储操作", request.Method)
			response.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(context.Background(), storageTestConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestPutUploadContentResults(t *testing.T) {
	for _, fixture := range []struct {
		name, etag, version string
		payload             []byte
	}{
		{"empty", "empty-etag", "", nil},
		{"binary", "opaque+etag", "version+id", []byte{0, 0xff, 0xfe, 1}},
		{"UTF8", "opaque", "null", []byte("学习资料 🌱")},
		{"optional metadata", "", "", []byte("abc")},
		{"SINGLE limit", "large-etag", "", bytes.Repeat([]byte{0xab}, int(domain.MaxSingleUploadSizeBytes))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var calls atomic.Int32
			client := uploadContentClient(t, func(response http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				// SDK 默认签名使用 aws-chunked，解码后核对实际文件字节。
				var body io.Reader = request.Body
				if strings.Contains(request.Header.Get("Content-Encoding"), "aws-chunked") {
					body = httputil.NewChunkedReader(body)
				}
				payload, err := io.ReadAll(body)
				checksum := md5.Sum(fixture.payload)
				if err != nil || !bytes.Equal(payload, fixture.payload) || request.URL.Path != "/cloudnest/users/7/objects/fixture" || request.Header.Get("X-Amz-Decoded-Content-Length") != strconv.Itoa(len(fixture.payload)) || request.Header.Get("Content-Type") != "application/octet-stream" || request.Header.Get("If-None-Match") != "*" || request.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(checksum[:]) || !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("单次签名 PUT 的内容、条件或长度错误", err)
				}
				if fixture.etag != "" {
					response.Header().Set("ETag", "\""+fixture.etag+"\"")
				}
				if fixture.version != "" {
					response.Header().Set("X-Amz-Version-Id", fixture.version)
				}
				response.WriteHeader(http.StatusOK)
			})
			file, err := os.Create(filepath.Join(t.TempDir(), "content.part"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write(fixture.payload); err != nil {
				t.Fatal(err)
			}
			result, err := client.PutUploadContent(context.Background(), "users/7/objects/fixture", file, uint64(len(fixture.payload)))
			if err != nil || result == nil || result.SizeBytes != uint64(len(fixture.payload)) || calls.Load() != 1 {
				t.Fatal("存储确认结果错误", result, err)
			}
			if (result.StorageETag == nil) != (fixture.etag == "") || (result.StorageETag != nil && *result.StorageETag != fixture.etag) || (result.StorageVersionID == nil) != (fixture.version == "") || (result.StorageVersionID != nil && *result.StorageVersionID != fixture.version) {
				t.Fatal("可空存储元数据错误")
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal("SDK 关闭了调用方文件", err)
			}
			payload, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(payload, fixture.payload) {
				t.Fatal("调用方文件不可继续读取", err)
			}
		})
	}
}

type uploadSeekSource struct {
	reader               *bytes.Reader
	reads, seeks, closes int
	readError, seekError error
	failStart            bool
	afterSeek            func()
}

func (source *uploadSeekSource) Read(buffer []byte) (int, error) {
	source.reads++
	if source.readError != nil {
		return 0, source.readError
	}
	return source.reader.Read(buffer)
}

func (source *uploadSeekSource) Seek(offset int64, whence int) (int64, error) {
	source.seeks++
	if source.seekError != nil && (!source.failStart || whence == io.SeekStart) {
		return 0, source.seekError
	}
	position, err := source.reader.Seek(offset, whence)
	if source.afterSeek != nil {
		source.afterSeek()
	}
	return position, err
}

func (source *uploadSeekSource) Close() error { source.closes++; return nil }

func TestPutUploadContentInputs(t *testing.T) {
	var calls atomic.Int32
	client := uploadContentClient(t, func(response http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		response.WriteHeader(http.StatusOK)
	})
	localFailure := errors.New("private-local-file-failure")
	for _, name := range []string{"nil client", "zero client", "nil context", "nil source", "empty key", "blank key", "long key", "Unicode key", "control key", "oversized", "max uint64", "short", "long", "seek end failure", "seek start failure", "read failure", "canceled", "cancel after seek"} {
		t.Run(name, func(t *testing.T) {
			current := client
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var inputContext context.Context = ctx
			source := &uploadSeekSource{reader: bytes.NewReader([]byte("abc"))}
			var reader io.ReadSeeker = source
			key, size := "users/7/objects/fixture", uint64(3)
			var want error
			switch name {
			case "nil client":
				current = nil
			case "zero client":
				current = &Client{}
			case "nil context":
				inputContext = nil
			case "nil source":
				reader = nil
			case "empty key", "blank key", "long key", "Unicode key", "control key":
				key = map[string]string{"empty key": "", "blank key": "   ", "long key": strings.Repeat("x", 256), "Unicode key": "用户/fixture", "control key": "private-key\nfixture"}[name]
				want = domain.ErrInvalidStoredObjectData
			case "oversized":
				size, want = domain.MaxSingleUploadSizeBytes+1, domain.ErrUploadFileTooLarge
			case "max uint64":
				size, want = math.MaxUint64, domain.ErrUploadFileTooLarge
			case "short":
				size, want = 4, domain.ErrUploadBodySizeMismatch
			case "long":
				size, want = 2, domain.ErrUploadBodySizeMismatch
			case "seek end failure", "seek start failure":
				source.seekError, source.failStart, want = localFailure, name == "seek start failure", localFailure
			case "read failure":
				source.readError, want = localFailure, domain.ErrUploadStorageUnavailable
			case "canceled":
				cancel()
				want = context.Canceled
			case "cancel after seek":
				source.afterSeek, want = cancel, context.Canceled
			}
			result, err := current.PutUploadContent(inputContext, key, reader, size)
			if err == nil || result != nil || (want != nil && !errors.Is(err, want)) || calls.Load() != 0 || (name != "read failure" && source.reads != 0) || source.closes != 0 {
				t.Fatal("输入拒绝、原始原因或源文件所有权错误", err)
			}
			if name == "read failure" && (source.reads == 0 || strings.Contains(err.Error(), "private-local-file-failure")) {
				t.Fatal("读取失败未被覆盖或包含原始诊断")
			}
		})
	}
}

func TestPutUploadContentFailures(t *testing.T) {
	for _, name := range []string{"exists", "forbidden", "server failure", "long ETag", "long version", "Unicode metadata", "canceled during PUT", "deadline"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var inputContext context.Context = ctx
			want := domain.ErrUploadStorageUnavailable
			if name == "exists" {
				want = domain.ErrUploadObjectExists
			}
			if name == "canceled during PUT" {
				want = context.Canceled
			}
			if name == "deadline" {
				want = context.DeadlineExceeded
			}
			client := uploadContentClient(t, func(response http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				switch name {
				case "exists", "forbidden", "server failure":
					response.Header().Set("Content-Type", "application/xml")
					status := map[string]int{"exists": 412, "forbidden": 403, "server failure": 500}[name]
					response.WriteHeader(status)
					_, _ = io.WriteString(response, "<Error><Code>FixtureFailure</Code><Message>private-storage-diagnostic</Message></Error>")
				case "long ETag":
					response.Header().Set("ETag", strings.Repeat("x", 256))
				case "long version":
					response.Header().Set("X-Amz-Version-Id", strings.Repeat("v", 256))
				case "Unicode metadata":
					response.Header().Set("ETag", "元数据")
				case "canceled during PUT":
					cancel()
				case "deadline":
					<-request.Context().Done()
				}
			})
			file, err := os.Create(filepath.Join(t.TempDir(), "content.part"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString("abc"); err != nil {
				t.Fatal(err)
			}
			if name == "deadline" {
				var deadlineCancel context.CancelFunc
				inputContext, deadlineCancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer deadlineCancel()
			}
			result, err := client.PutUploadContent(inputContext, "users/7/objects/fixture", file, 3)
			if !errors.Is(err, want) || result != nil || strings.Contains(err.Error(), "private-storage-diagnostic") {
				t.Fatal("存储失败分类、取消或诊断保护错误", err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal("错误路径关闭了源文件", err)
			}
		})
	}
}
