package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// SingleUploadContentTarget 保存正文请求需要的远端任务、路径和声明大小。
type SingleUploadContentTarget struct {
	TaskID      string
	ContentPath string
	SizeBytes   int64
}

// ReadSingleUploadStatus 在发送前对账，避免重发已经接收的正文。
func ReadSingleUploadStatus(ctx context.Context, address, accessToken string, target SingleUploadContentTarget) (string, error) {
	if !validEntryID(target.TaskID) {
		return "", errors.New("无效的上传任务 ID")
	}
	var result struct {
		Data struct {
			ID         string `json:"id"`
			UploadMode string `json:"upload_mode"`
			SizeBytes  *int64 `json:"size_bytes"`
			Status     string `json:"status"`
		} `json:"data"`
	}
	if err := getAuthenticatedJSON(ctx, address, accessToken, "uploads/"+target.TaskID, nil, &result); err != nil {
		return "", err
	}
	task := result.Data
	if task.ID != target.TaskID || task.UploadMode != "SINGLE" ||
		task.SizeBytes == nil || *task.SizeBytes != target.SizeBytes || task.Status == "" {
		return "", errors.New("远端任务与本机正文声明不一致")
	}
	return task.Status, nil
}

// SendSingleUploadContent 流式发送原始文件字节，并核对接收回执。
func SendSingleUploadContent(ctx context.Context, address, accessToken string, target SingleUploadContentTarget, source io.Reader) error {
	// 1. 确认正文地址与当前任务一致，再构造已知长度的 PUT。
	if !validEntryID(target.TaskID) || target.ContentPath != "/api/v1/uploads/"+target.TaskID+"/content" ||
		target.SizeBytes < 0 || target.SizeBytes > MaxSingleUploadBytes || (target.SizeBytes > 0 && source == nil) {
		return errors.New("无效的 SINGLE 正文目标")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return err
	}
	body := io.Reader(http.NoBody)
	if target.SizeBytes > 0 {
		body = io.LimitReader(source, target.SizeBytes)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, base.JoinPath(target.ContentPath).String(), body)
	if err != nil {
		return errors.New("无法构造正文请求")
	}
	request.ContentLength = target.SizeBytes
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/octet-stream")

	// 2. 沿用服务客户端的连接与跳转策略，给正文接收留出服务端时限。
	contentClient := serviceHTTPClient
	contentClient.Timeout = 5*time.Minute + 10*time.Second
	response, err := contentClient.Do(request)
	if err != nil {
		return errors.New("未确认正文接收结果，再次发送前会查询远端状态")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		code := authErrorCode(response.Body)
		if response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED" {
			return ErrAccessTokenExpired
		}
		if code == "BODY_SIZE_MISMATCH" || code == "CHECKSUM_MISMATCH" {
			return errors.New("后端核对发现正文大小或摘要不符，请检查源文件")
		}
		return errors.New("服务端未确认正文接收，再次发送前会查询远端状态")
	}

	// 3. 只有当前任务的 VERIFYING 回执才能确认发送完成。
	var result struct {
		Data struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil ||
		result.Data.TaskID != target.TaskID || result.Data.Status != "VERIFYING" {
		return errors.New("正文回执与当前任务不一致，再次发送前会查询远端状态")
	}
	return nil
}
