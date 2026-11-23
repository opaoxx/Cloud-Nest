package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// MaxSingleUploadBytes is the server's upper bound for SINGLE creation.
const MaxSingleUploadBytes = 16 * 1024 * 1024

// SingleUploadDeclaration is the immutable JSON sent when creating a SINGLE task.
type SingleUploadDeclaration struct {
	ParentID  string `json:"parent_id"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// SingleUploadCreation describes the confirmed remote task and its content endpoint.
type SingleUploadCreation struct {
	TaskID      string
	ContentPath string
	ExpiresAt   time.Time
}

// CreateSingleUpload submits the stored declaration with its original creation key.
func CreateSingleUpload(ctx context.Context, address, accessToken, creationKey string, declaration SingleUploadDeclaration) (SingleUploadCreation, error) {
	// 1. 本步骤只创建 SINGLE，防止大文件被服务端自动选为分片上传。
	if declaration.SizeBytes < 0 || declaration.SizeBytes > MaxSingleUploadBytes {
		return SingleUploadCreation{}, errors.New("SINGLE 支持 0 至 16 MiB 的文件")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return SingleUploadCreation{}, err
	}
	body, err := json.Marshal(declaration)
	if err != nil {
		return SingleUploadCreation{}, errors.New("无法编码上传声明")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("api", "v1", "uploads").String(), bytes.NewReader(body))
	if err != nil {
		return SingleUploadCreation{}, errors.New("无法构造上传创建请求")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Idempotency-Key", creationKey)
	// 创建请求由本机记录控制重试，禁用 HTTP 传输层对正文的自动重放。
	request.GetBody = nil

	// 2. 等待创建回执。未确认时由调用方保留声明和原键。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return SingleUploadCreation{}, errors.New("未收到上传创建回执，请重试同一条记录")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return SingleUploadCreation{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusConflict && code == "IDEMPOTENCY_CONFLICT":
			return SingleUploadCreation{}, errors.New("服务端记录的上传声明与本机不一致")
		case response.StatusCode == http.StatusConflict && code == "IDEMPOTENCY_EXPIRED":
			return SingleUploadCreation{}, errors.New("服务端的创建键已过期，无法找回原任务")
		case response.StatusCode == http.StatusNotFound:
			return SingleUploadCreation{}, errors.New("上传目标目录不存在或已不可访问")
		default:
			return SingleUploadCreation{}, errors.New("服务端未确认创建上传任务")
		}
	}

	// 3. 核对任务、真实目标和正文路径，避免将错误回执写入本机。
	var result struct {
		Data struct {
			Task struct {
				ID         string    `json:"id"`
				ParentID   string    `json:"parent_id"`
				SizeBytes  *int64    `json:"size_bytes"`
				UploadMode string    `json:"upload_mode"`
				Status     string    `json:"status"`
				ExpiresAt  time.Time `json:"expires_at"`
			} `json:"task"`
			UploadMode  string    `json:"upload_mode"`
			ContentPath string    `json:"content_path"`
			ExpiresAt   time.Time `json:"expires_at"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil {
		return SingleUploadCreation{}, errors.New("无法读取上传创建回执，请重试同一条记录")
	}
	data := result.Data
	if !validEntryID(data.Task.ID) || data.Task.ParentID != declaration.ParentID ||
		data.Task.SizeBytes == nil || *data.Task.SizeBytes != declaration.SizeBytes ||
		data.UploadMode != "SINGLE" || data.Task.UploadMode != "SINGLE" || data.Task.Status != "UPLOADING" ||
		data.ContentPath != "/api/v1/uploads/"+data.Task.ID+"/content" ||
		data.ExpiresAt.IsZero() || !data.ExpiresAt.Equal(data.Task.ExpiresAt) {
		return SingleUploadCreation{}, errors.New("上传创建回执与本机声明不一致")
	}
	return SingleUploadCreation{TaskID: data.Task.ID, ContentPath: data.ContentPath, ExpiresAt: data.ExpiresAt.UTC()}, nil
}
