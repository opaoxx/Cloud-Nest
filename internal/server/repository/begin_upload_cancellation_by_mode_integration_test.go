package repository

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestBeginUserUploadCancellationByModeIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const singleSize, multipartSize, partSize = 16 << 20, (16 << 20) + 1, 16 << 20
	const testPath = "/api/v1/uploads/19/cancel?upload_mode=unexpected&user_id=8"
	const ownerCredential, foreignCredential = "fixture-owner", "fixture-foreign"
	owner := domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
	foreign := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 22}
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}

	// 1. 在独占库的回环 HTTP 实例装配现有受保护路由和按模式受理方法。
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	serverhttp.RegisterUploadCancellationRoute(router, func(_ context.Context, token string) (*domain.Actor, error) {
		switch token {
		case ownerCredential:
			return &owner, nil
		case foreignCredential:
			return &foreign, nil
		default:
			return nil, domain.ErrInvalidAccessToken
		}
	}, store.BeginUserUploadCancellationByMode)
	server := httptest.NewServer(router)
	defer server.Close()
	request := func(t *testing.T, credential string, status int, code string) dto.UploadTaskResponse {
		t.Helper()
		body := strings.NewReader(`{"upload_mode":"unexpected","user_id":"8"}`)
		message, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+testPath, body)
		if err != nil {
			t.Fatal(err)
		}
		if credential != "" {
			message.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := server.Client().Do(message)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		requestID := response.Header.Get("X-Request-ID")
		if err != nil || response.StatusCode != status || requestID == "" || response.Header.Get("Cache-Control") != "no-store" ||
			strings.Contains(string(raw), "private-session") {
			t.Fatal("取消 HTTP 响应不符", response.StatusCode, string(raw), err)
		}
		if code != "" {
			var failure dto.ErrorResponse
			if json.Unmarshal(raw, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID {
				t.Fatal("取消错误契约不符", string(raw))
			}
			return dto.UploadTaskResponse{}
		}
		var result dto.UploadTaskResponse
		if json.Unmarshal(raw, &result) != nil || result.Data.ID != "19" || result.RequestID != requestID {
			t.Fatal("取消成功契约不符", string(raw))
		}
		return result
	}

	for _, fixture := range []struct {
		mode      string
		fileSize  uint64
		partSize  any
		partCount any
		session   any
	}{
		{mode: "SINGLE", fileSize: singleSize},
		{mode: "MULTIPART", fileSize: multipartSize, partSize: partSize, partCount: 2, session: "private-session+A/= "},
	} {
		t.Run(fixture.mode, func(t *testing.T) {
			// 2. 使用任务记录的实际模式受理，两种模式均先检查身份、归属及租约。
			execute(t, "UPDATE users SET used_storage=1024,reserved_storage=? WHERE id=7", fixture.fileSize+512)
			execute(t, "UPDATE stored_objects SET file_size=? WHERE id=23", fixture.fileSize)
			execute(t, `UPDATE upload_tasks SET upload_mode=?,status='UPLOADING',file_size=?,part_size=?,part_count=?,storage_upload_id=?,
				expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,published_object_id=NULL,result_entry_id=NULL,resolved_name=NULL,
				completed_at=NULL,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,next_attempt_at=NULL,
				lease_owner='fixture-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19`,
				fixture.mode, fixture.fileSize, fixture.partSize, fixture.partCount, fixture.session, fixture.fileSize)
			before := read(t)
			request(t, "", http.StatusUnauthorized, "AUTH_REQUIRED")
			request(t, foreignCredential, http.StatusNotFound, "UPLOAD_NOT_FOUND")
			request(t, ownerCredential, http.StatusConflict, "UPLOAD_BUSY")
			if !reflect.DeepEqual(before, read(t)) {
				t.Fatal("拒绝的请求改变了任务")
			}
			execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND, expires_at=UTC_TIMESTAMP(6) WHERE id=19")
			before = read(t)
			request(t, ownerCredential, http.StatusConflict, "TASK_NOT_READY")
			if !reflect.DeepEqual(before, read(t)) {
				t.Fatal("到期冲突改变了任务")
			}
			execute(t, "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19")
			before = read(t)
			accepted := request(t, ownerCredential, http.StatusAccepted, "")
			updated := read(t)
			expected := *before
			expected.Status, expected.UpdatedAt = domain.UploadStatusCancelling, updated.UpdatedAt
			expected.LeaseOwner, expected.LeaseUntil = nil, nil
			replayed := request(t, ownerCredential, http.StatusAccepted, "")
			if accepted.Data.UploadMode != fixture.mode || accepted.Data.Status != "CANCELLING" ||
				!reflect.DeepEqual(accepted.Data, replayed.Data) || !reflect.DeepEqual(updated, &expected) || !reflect.DeepEqual(updated, read(t)) {
				t.Fatal("取消受理未保留模式、原会话和其他任务字段")
			}
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil ||
				used != 1024 || reserved != fixture.fileSize+512 {
				t.Fatal("受理阶段改变了账号账目", used, reserved, err)
			}

			// 3. 读取已有的已发布终态，返回原结果 ID 并保持任务不变。
			execute(t, `UPDATE upload_tasks SET status='COMPLETED',reserved_bytes=0,published_object_id=29,result_entry_id=31,
				resolved_name='published.md',completed_at=UTC_TIMESTAMP(6) WHERE id=19`)
			completed := read(t)
			terminal := request(t, ownerCredential, http.StatusOK, "")
			if terminal.Data.Status != "COMPLETED" || terminal.Data.UploadMode != fixture.mode || terminal.Data.ResultEntryID == nil ||
				*terminal.Data.ResultEntryID != "31" || !reflect.DeepEqual(completed, read(t)) {
				t.Fatal("已有发布结果未复用")
			}
			t.Logf("%s 验证 401、404、两个 409、首次及重复 202、终态 200，受理保持原预占 %d B", fixture.mode, reserved)
		})
	}
}
