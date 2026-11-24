package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ErrMultipartCompletionUnknown means the task may have accepted the completion request.
var ErrMultipartCompletionUnknown = errors.New("未确认完成请求的结果，请查看原任务的发布结果")

// ReadMultipartUploadTask reads the current state of the same Multipart task.
func ReadMultipartUploadTask(ctx context.Context, address, accessToken, taskID, parentID string, sizeBytes int64) (UploadTaskSnapshot, error) {
	return readUploadTaskSnapshot(ctx, address, accessToken, taskID, "MULTIPART", sizeBytes, parentID)
}

// CompleteMultipartUpload submits a full ordered set of part receipts for the existing task.
func CompleteMultipartUpload(ctx context.Context, address, accessToken, taskID, parentID string, plan MultipartPlan, receipts []UploadPartReceipt) (UploadTaskSnapshot, error) {
	// 1. 逐一核对本机回执。完整计划和实际字节数必须覆盖所有片号。
	if !validEntryID(taskID) || !validEntryID(parentID) || len(receipts) != plan.PartCount || len(receipts) == 0 || len(receipts) > maxUploadParts {
		return UploadTaskSnapshot{}, errors.New("分片回执不完整，请先核对并续传")
	}
	type completionPart struct {
		Number int    `json:"part_number"`
		ETag   string `json:"etag"`
	}
	parts := make([]completionPart, 0, len(receipts))
	for index, receipt := range receipts {
		expected, err := plan.PartRange(index + 1)
		if err != nil || receipt.Number != index+1 || receipt.SizeBytes != expected.SizeBytes ||
			!uploadPartETagPattern.MatchString(receipt.ETag) {
			return UploadTaskSnapshot{}, errors.New("分片回执与计划不一致，请先核对并续传")
		}
		parts = append(parts, completionPart{Number: receipt.Number, ETag: receipt.ETag})
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return UploadTaskSnapshot{}, err
	}
	body, err := json.Marshal(struct {
		Parts []completionPart `json:"parts"`
	}{Parts: parts})
	if err != nil {
		return UploadTaskSnapshot{}, errors.New("无法编码有序分片回执")
	}
	endpoint := base.JoinPath("api", "v1", "uploads", taskID, "complete")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return UploadTaskSnapshot{}, errors.New("无法构造上传完成请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.GetBody = nil

	// 2. POST 投递状态未知时由调用方查询原任务。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return UploadTaskSnapshot{}, ErrMultipartCompletionUnknown
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return UploadTaskSnapshot{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusConflict && code == "UPLOAD_PARTS_MISMATCH":
			return UploadTaskSnapshot{}, errors.New("服务端核对分片回执不一致，请先核对并续传")
		case response.StatusCode == http.StatusConflict && code == "UPLOAD_BUSY":
			return UploadTaskSnapshot{}, errors.New("任务正在处理，请稍后查看发布结果")
		case response.StatusCode >= http.StatusInternalServerError:
			return UploadTaskSnapshot{}, ErrMultipartCompletionUnknown
		default:
			return UploadTaskSnapshot{}, errors.New("服务端未接受上传完成请求")
		}
	}

	// 3. 202 只代表受理，200 必须交付同一任务已发布的条目和完成时间。
	var result struct {
		Data uploadTask `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil {
		return UploadTaskSnapshot{}, ErrMultipartCompletionUnknown
	}
	task := result.Data
	if task.ID != taskID || task.UploadMode != "MULTIPART" || task.SizeBytes == nil || *task.SizeBytes != plan.SizeBytes {
		return UploadTaskSnapshot{}, ErrMultipartCompletionUnknown
	}
	snapshot, err := uploadTaskSnapshot(task, parentID)
	if err != nil ||
		(response.StatusCode == http.StatusOK && (snapshot.Status != "COMPLETED" || task.CompletedAt == nil || task.CompletedAt.IsZero())) ||
		(response.StatusCode == http.StatusAccepted && ((snapshot.Status != "COMPLETING" && snapshot.Status != "VERIFYING") || task.CompletedAt != nil)) {
		return UploadTaskSnapshot{}, ErrMultipartCompletionUnknown
	}
	return snapshot, nil
}
