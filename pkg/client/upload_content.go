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
	task, err := readUploadTask(ctx, address, accessToken, target.TaskID, "SINGLE", target.SizeBytes)
	return task.Status, err
}

// SingleUploadTaskSnapshot 保存校验和发布阶段供客户端使用的公开字段。
type SingleUploadTaskSnapshot struct {
	Status        string
	ResultEntryID string
	ErrorCode     string
	ErrorMessage  string
}

type uploadTask struct {
	ID            string    `json:"id"`
	ParentID      string    `json:"parent_id"`
	UploadMode    string    `json:"upload_mode"`
	SizeBytes     *int64    `json:"size_bytes"`
	Status        string    `json:"status"`
	ResultEntryID string    `json:"result_entry_id"`
	ExpiresAt     time.Time `json:"expires_at"`
	LastError     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"last_error"`
}

// ReadSingleUploadTask 查询一个 SINGLE 任务并核对发布结果。
func ReadSingleUploadTask(ctx context.Context, address, accessToken string, target SingleUploadContentTarget, parentID string) (SingleUploadTaskSnapshot, error) {
	// 1. 复用同一任务的认证查询，再核对已保存的目标目录。
	task, err := readUploadTask(ctx, address, accessToken, target.TaskID, "SINGLE", target.SizeBytes)
	if err != nil {
		return SingleUploadTaskSnapshot{}, err
	}
	if !validEntryID(parentID) || parentID != task.ParentID {
		return SingleUploadTaskSnapshot{}, errors.New("远端任务的目标目录与本机记录不一致")
	}

	// 2. COMPLETED 必须有有效条目 ID，其余阶段不得宣告已发布。
	switch task.Status {
	case "CREATING", "UPLOADING", "COMPLETING", "VERIFYING", "COMPLETED", "CANCELLING", "CANCELLED", "EXPIRING", "EXPIRED", "FAILED":
	default:
		return SingleUploadTaskSnapshot{}, errors.New("远端上传任务状态不受支持")
	}
	if (task.Status == "COMPLETED" && !validEntryID(task.ResultEntryID)) ||
		(task.Status != "COMPLETED" && task.ResultEntryID != "") {
		return SingleUploadTaskSnapshot{}, errors.New("远端任务的发布条目无效")
	}
	snapshot := SingleUploadTaskSnapshot{Status: task.Status, ResultEntryID: task.ResultEntryID}
	if task.LastError != nil {
		snapshot.ErrorCode = task.LastError.Code
		snapshot.ErrorMessage = task.LastError.Message
	}
	return snapshot, nil
}

func readUploadTask(ctx context.Context, address, accessToken, taskID, mode string, sizeBytes int64) (uploadTask, error) {
	if !validEntryID(taskID) {
		return uploadTask{}, errors.New("无效的上传任务 ID")
	}
	var result struct {
		Data uploadTask `json:"data"`
	}
	if err := getAuthenticatedJSON(ctx, address, accessToken, "uploads/"+taskID, nil, &result); err != nil {
		return uploadTask{}, err
	}
	task := result.Data
	if task.ID != taskID || task.UploadMode != mode ||
		task.SizeBytes == nil || *task.SizeBytes != sizeBytes || task.Status == "" {
		return uploadTask{}, errors.New("远端任务与本机正文声明不一致")
	}
	return task, nil
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
