package dto

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewUploadContentAcceptedDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, fixture := range []struct {
		id   uint64
		text string
	}{
		{1, "1"},
		{19, "19"},
		{9007199254740993, "9007199254740993"},
		{^uint64(0), "18446744073709551615"},
	} {
		t.Run(fixture.text, func(t *testing.T) {
			// 使用完整任务夹具，核对响应只包含公开字段。
			task := uploadTaskDataFixture()
			task.ID, task.Status = fixture.id, domain.UploadStatusVerifying
			data, err := NewUploadContentAcceptedData(&task)
			if err != nil || data == nil {
				t.Fatal("有效任务未转换为接收结果", err)
			}
			encoded, err := json.Marshal(UploadContentAcceptedResponse{Data: *data, RequestID: requestID})
			want := `{"data":{"task_id":"` + fixture.text + `","status":"VERIFYING"},"request_id":"` + requestID + `"}`
			if err != nil || string(encoded) != want {
				t.Fatal("接收响应 JSON 字段或 ID 错误", string(encoded), err)
			}
		})
	}
}

func TestNewUploadContentAcceptedDataRejectsInvalidTask(t *testing.T) {
	for _, fixture := range []struct {
		name string
		task *domain.UploadTask
	}{
		{"missing task", nil},
		{"zero ID", &domain.UploadTask{Status: domain.UploadStatusVerifying}},
		{"still uploading", &domain.UploadTask{ID: 19, Status: domain.UploadStatusUploading}},
		{"already completed", &domain.UploadTask{ID: 19, Status: domain.UploadStatusCompleted}},
		{"unknown phase", &domain.UploadTask{ID: 19, Status: "verifying"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			data, err := NewUploadContentAcceptedData(fixture.task)
			if data != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
				t.Fatal("非法任务应返回空响应数据与任务数据错误", data, err)
			}
		})
	}
}
