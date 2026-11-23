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

// UploadDeclaration is the immutable JSON sent when creating an upload task.
type UploadDeclaration struct {
	ParentID  string `json:"parent_id"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// SingleUploadDeclaration is the declaration accepted by CreateSingleUpload.
type SingleUploadDeclaration = UploadDeclaration

// SingleUploadCreation describes the confirmed remote task and its content endpoint.
type SingleUploadCreation struct {
	TaskID      string
	ContentPath string
	ExpiresAt   time.Time
}

// CreateSingleUpload submits the stored declaration with its original creation key.
func CreateSingleUpload(ctx context.Context, address, accessToken, creationKey string, declaration SingleUploadDeclaration) (SingleUploadCreation, error) {
	if declaration.SizeBytes < 0 || declaration.SizeBytes > MaxSingleUploadBytes {
		return SingleUploadCreation{}, errors.New("SINGLE 支持 0 至 16 MiB 的文件")
	}
	receipt, err := CreateUpload(ctx, address, accessToken, creationKey, declaration)
	if err != nil {
		return SingleUploadCreation{}, err
	}
	return SingleUploadCreation{TaskID: receipt.TaskID, ContentPath: receipt.ContentPath, ExpiresAt: receipt.ExpiresAt}, nil
}

// UploadCreation contains the task identity and mode-specific creation receipt.
type UploadCreation struct {
	TaskID      string
	ContentPath string
	ExpiresAt   time.Time
	Multipart   *MultipartPlan
}

// CreateUpload submits the stored declaration and validates the server-selected mode.
func CreateUpload(ctx context.Context, address, accessToken, creationKey string, declaration UploadDeclaration) (UploadCreation, error) {
	// 1. 沿用本机声明和创建键，上传模式由服务端按文件大小选择。
	if declaration.SizeBytes < 0 || declaration.SizeBytes > maxJSONSafeInteger {
		return UploadCreation{}, errors.New("文件大小超出上传协议支持的范围")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return UploadCreation{}, err
	}
	body, err := json.Marshal(declaration)
	if err != nil {
		return UploadCreation{}, errors.New("无法编码上传声明")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("api", "v1", "uploads").String(), bytes.NewReader(body))
	if err != nil {
		return UploadCreation{}, errors.New("无法构造上传创建请求")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Idempotency-Key", creationKey)
	// 创建请求由本机记录控制重试，禁用 HTTP 传输层对正文的自动重放。
	request.GetBody = nil

	// 2. 等待创建回执。未确认时由调用方保留声明和原键。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return UploadCreation{}, errors.New("未收到上传创建回执，请重试同一条记录")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return UploadCreation{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusConflict && code == "IDEMPOTENCY_CONFLICT":
			return UploadCreation{}, errors.New("服务端记录的上传声明与本机不一致")
		case response.StatusCode == http.StatusConflict && code == "IDEMPOTENCY_EXPIRED":
			return UploadCreation{}, errors.New("服务端的创建键已过期，无法找回原任务")
		case code == "FILE_TOO_LARGE":
			return UploadCreation{}, errors.New("文件超过服务端允许上传的大小")
		case response.StatusCode == http.StatusNotFound:
			return UploadCreation{}, errors.New("上传目标目录不存在或已不可访问")
		default:
			return UploadCreation{}, errors.New("服务端未确认创建上传任务")
		}
	}

	// 3. 核对任务、真实目标和固定计划，再交付可保存的创建回执。
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
			UploadMode    string    `json:"upload_mode"`
			ContentPath   string    `json:"content_path"`
			PartSizeBytes int64     `json:"part_size_bytes"`
			PartCount     int       `json:"part_count"`
			ExpiresAt     time.Time `json:"expires_at"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil {
		return UploadCreation{}, errors.New("无法读取上传创建回执，请重试同一条记录")
	}
	data := result.Data
	if !validEntryID(data.Task.ID) || data.Task.ParentID != declaration.ParentID ||
		data.Task.SizeBytes == nil || *data.Task.SizeBytes != declaration.SizeBytes ||
		data.UploadMode != data.Task.UploadMode ||
		data.ExpiresAt.IsZero() || !data.ExpiresAt.Equal(data.Task.ExpiresAt) {
		return UploadCreation{}, errors.New("上传创建回执与本机声明不一致")
	}
	receipt := UploadCreation{TaskID: data.Task.ID, ExpiresAt: data.ExpiresAt.UTC()}
	if declaration.SizeBytes <= MaxSingleUploadBytes {
		if data.UploadMode != "SINGLE" || data.Task.Status != "UPLOADING" ||
			data.ContentPath != "/api/v1/uploads/"+data.Task.ID+"/content" || data.PartSizeBytes != 0 || data.PartCount != 0 {
			return UploadCreation{}, errors.New("SINGLE 创建回执与本机声明不一致")
		}
		receipt.ContentPath = data.ContentPath
		return receipt, nil
	}
	if data.UploadMode != "MULTIPART" || data.Task.Status != "CREATING" || data.ContentPath != "" {
		return UploadCreation{}, errors.New("Multipart 创建回执与本机声明不一致")
	}
	plan := MultipartPlan{SizeBytes: declaration.SizeBytes, PartSizeBytes: data.PartSizeBytes, PartCount: data.PartCount}
	if _, err := plan.PartRange(plan.PartCount); err != nil {
		return UploadCreation{}, err
	}
	receipt.Multipart = &plan
	return receipt, nil
}
