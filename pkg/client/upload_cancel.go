package client

import (
	"context"
	"errors"
	"net/http"
)

// UploadTaskTarget 保存核对同一个远端上传任务需要的声明。
type UploadTaskTarget struct {
	TaskID    string
	ParentID  string
	Mode      string
	SizeBytes int64
}

// ReadUploadTask 查询原任务并核对其声明和发布结果。
func ReadUploadTask(ctx context.Context, address, accessToken string, target UploadTaskTarget) (UploadTaskSnapshot, error) {
	return readUploadTaskSnapshot(ctx, address, accessToken, target.TaskID, target.Mode, target.SizeBytes, target.ParentID)
}

var errUploadCancellationUnknown = errors.New("取消请求待确认，请查看原任务的取消结果")

// CancelUploadTask 提交无正文取消请求，返回后台受理状态或已有终态。
func CancelUploadTask(ctx context.Context, address, accessToken string, target UploadTaskTarget) (UploadTaskSnapshot, error) {
	// 1. 从已保存的任务 ID 构造请求，沿用服务客户端的连接与跳转策略。
	if !validEntryID(target.TaskID) || !validEntryID(target.ParentID) || target.SizeBytes < 0 ||
		(target.Mode != "SINGLE" && target.Mode != "MULTIPART") {
		return UploadTaskSnapshot{}, errors.New("无效的上传取消目标")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return UploadTaskSnapshot{}, err
	}
	endpoint := base.JoinPath("api", "v1", "uploads", target.TaskID, "cancel")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return UploadTaskSnapshot{}, errors.New("无法构造上传取消请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)

	// 2. 区分服务端明确拒绝和结果未确认，调用方使用 GET 核对原任务。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return UploadTaskSnapshot{}, errUploadCancellationUnknown
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return UploadTaskSnapshot{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusConflict && code == "UPLOAD_BUSY":
			return UploadTaskSnapshot{}, errors.New("服务端仍在处理任务，请稍后查询或重试取消")
		case response.StatusCode == http.StatusConflict && code == "TASK_NOT_READY":
			return UploadTaskSnapshot{}, errors.New("服务端当前无法受理取消，请查看任务的实际结果")
		case response.StatusCode >= http.StatusInternalServerError:
			return UploadTaskSnapshot{}, errUploadCancellationUnknown
		default:
			return UploadTaskSnapshot{}, errors.New("服务端未接受上传取消请求")
		}
	}

	// 3. 验证原任务身份和状态，202 只表示受理，200 必须给出实际终态。
	var result struct {
		Data uploadTask `json:"data"`
	}
	if err := readSmallJSON(response.Body, &result); err != nil {
		return UploadTaskSnapshot{}, errUploadCancellationUnknown
	}
	task := result.Data
	if task.ID != target.TaskID || task.UploadMode != target.Mode || task.SizeBytes == nil || *task.SizeBytes != target.SizeBytes {
		return UploadTaskSnapshot{}, errUploadCancellationUnknown
	}
	snapshot, err := uploadTaskSnapshot(task, target.ParentID)
	if err != nil {
		return UploadTaskSnapshot{}, errUploadCancellationUnknown
	}
	switch snapshot.Status {
	case "CANCELLING":
		if response.StatusCode == http.StatusAccepted && task.CompletedAt == nil {
			return snapshot, nil
		}
	case "CANCELLED", "COMPLETED", "FAILED", "EXPIRED":
		if response.StatusCode == http.StatusOK && (snapshot.Status != "COMPLETED" ||
			(task.CompletedAt != nil && !task.CompletedAt.IsZero())) {
			return snapshot, nil
		}
	}
	return UploadTaskSnapshot{}, errUploadCancellationUnknown
}
