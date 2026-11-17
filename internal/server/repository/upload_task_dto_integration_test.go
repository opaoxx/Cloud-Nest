package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadTaskDTOIntegration(t *testing.T) {
	// 1. 复用正式任务夹具，串联真实 Repository、Service 与响应转换。
	pool, want := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	read := func(t *testing.T, taskID uint64) *dto.UploadTaskData {
		t.Helper()
		internal, err := tasks.Get(ctx, actor, taskID)
		if err != nil {
			t.Fatal(err)
		}
		data, err := dto.NewUploadTaskData(internal)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(dto.UploadTaskResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
		if err != nil || strings.Contains(string(encoded), "private-upload") || strings.Contains(string(encoded), "曾经重试") {
			t.Fatal("任务 JSON 编码或公开摘要保护错误", err)
		}
		var object struct {
			Data      map[string]json.RawMessage `json:"data"`
			RequestID string                     `json:"request_id"`
		}
		if err := json.Unmarshal(encoded, &object); err != nil || len(object.Data) != 13 || object.RequestID != "28ee758a-0727-42de-ae7d-3bb33155e148" {
			t.Fatal("公开任务字段或请求 ID 错误", err)
		}
		return data
	}
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=? WHERE user_id=7 AND id=19", string(status)); err != nil {
				t.Fatal(err)
			}
			data := read(t, 19)
			if data.ID != "19" || data.ParentID != "13" || data.Status != string(status) || data.UploadMode != "MULTIPART" ||
				data.ResolvedName == nil || *data.ResolvedName != *want.ResolvedName || data.ResultEntryID == nil || *data.ResultEntryID != "31" ||
				data.CompletedAt == nil || !data.CompletedAt.Equal(*want.CompletedAt) || data.LastError == nil || data.LastError.Code != "INTERNAL_ERROR" {
				t.Fatal("正式任务状态、结果或未知内部码转换错误")
			}
		})
	}

	// 2. 已批准业务码和未知码均以固定摘要交付，原始诊断保留在数据库。
	for _, code := range []string{
		"TARGET_UNAVAILABLE", "BODY_SIZE_MISMATCH", "FILE_TOO_LARGE", "CHECKSUM_MISMATCH", "QUOTA_EXCEEDED",
		"TEMP_SPACE_EXHAUSTED", "STORAGE_UNAVAILABLE", "DATABASE_UNAVAILABLE", "INTERNAL_ERROR", "PRIVATE_STORAGE_RETRY",
	} {
		t.Run(code, func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET last_error_code=?,last_error_message=? WHERE user_id=7 AND id=19", code, "private-upload-diagnostic /object?token=fixture"); err != nil {
				t.Fatal(err)
			}
			data := read(t, 19)
			expectedCode := code
			if code == "PRIVATE_STORAGE_RETRY" {
				expectedCode = "INTERNAL_ERROR"
			}
			if data.LastError == nil || data.LastError.Code != expectedCode {
				t.Fatal("正式任务公开错误码转换错误")
			}
		})
	}
	t.Run("zero byte SINGLE and explicit nulls", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(25,7,'MINIO','fixture-bucket','zero-byte',0)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at,created_at,updated_at) VALUES(20,7,11,25,13,'empty.bin','SINGLE',0,'UPLOADING',0,?,?,?)", want.ExpiresAt, want.CreatedAt, want.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		data := read(t, 20)
		if data.SizeBytes != 0 || data.UploadMode != "SINGLE" || data.ResolvedName != nil || data.ResultEntryID != nil || data.CompletedAt != nil || data.LastError != nil {
			t.Fatal("正式零字节任务或空值转换错误")
		}
	})
	t.Run("JSON numeric boundary", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=9007199254740991 WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		if data := read(t, 19); data.SizeBytes != 9007199254740991 {
			t.Fatal("JSON 安全大小边界转换错误")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=9007199254740992 WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		internal, err := tasks.Get(ctx, actor, 19)
		if err != nil {
			t.Fatal(err)
		}
		data, err := dto.NewUploadTaskData(internal)
		if data != nil || err == nil {
			t.Fatal("JSON 不安全数值被交付")
		}
	})
}
