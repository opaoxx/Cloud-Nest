package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

// ErrReusableContentNotFound identifies the one response that permits a normal upload.
var ErrReusableContentNotFound = errors.New("当前账号没有可复用的内容")

// UploadReuseReceipt is the new directory entry created using verified storage.
type UploadReuseReceipt struct {
	EntryID string
	Name    string
}

// ReuseUploadContent tries to publish the given declaration from the current account's verified content.
func ReuseUploadContent(ctx context.Context, address, accessToken, creationKey string, declaration UploadDeclaration) (UploadReuseReceipt, error) {
	// 1. 复用上传意图的原值，使用原创建键向账号范围内的复用接口提交。
	digest, digestError := hex.DecodeString(declaration.SHA256)
	if !validEntryID(declaration.ParentID) || declaration.Name == "" || creationKey == "" ||
		declaration.SizeBytes < 0 || declaration.SizeBytes > maxJSONSafeInteger || digestError != nil || len(digest) != 32 {
		return UploadReuseReceipt{}, errors.New("无效的文件复用声明")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return UploadReuseReceipt{}, err
	}
	body, err := json.Marshal(declaration)
	if err != nil {
		return UploadReuseReceipt{}, errors.New("无法编码文件复用声明")
	}
	endpoint := base.JoinPath("api", "v1", "files", "reuse")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return UploadReuseReceipt{}, errors.New("无法构造文件复用请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", creationKey)
	request.GetBody = nil

	// 2. 只在明确未命中时向调用者交付回退信号。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return UploadReuseReceipt{}, errors.New("未收到文件复用回执，请重试同一条记录")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return UploadReuseReceipt{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusNotFound && code == "REUSABLE_CONTENT_NOT_FOUND":
			return UploadReuseReceipt{}, ErrReusableContentNotFound
		case response.StatusCode == http.StatusNotFound && code == "ENTRY_NOT_FOUND":
			return UploadReuseReceipt{}, errors.New("复用目标目录不存在或已不可访问")
		case response.StatusCode == http.StatusInsufficientStorage && code == "QUOTA_EXCEEDED":
			return UploadReuseReceipt{}, errors.New("账号剩余配额不足，无法发布该文件")
		case response.StatusCode == http.StatusConflict && code == "IDEMPOTENCY_EXPIRED":
			return UploadReuseReceipt{}, errors.New("服务端的复用创建键已过期，请先查看目标目录")
		case response.StatusCode == http.StatusConflict && code == "IDEMPOTENCY_CONFLICT":
			return UploadReuseReceipt{}, errors.New("服务端记录的复用声明与本机不一致")
		default:
			return UploadReuseReceipt{}, errors.New("服务端未确认文件复用，请重试同一条记录")
		}
	}

	// 3. 核对新文件条目的目录、大小、状态及初始修订，保留实际分配的名称。
	var result struct {
		Data struct {
			Entry struct {
				ID        string `json:"id"`
				ParentID  string `json:"parent_id"`
				Name      string `json:"name"`
				EntryType string `json:"entry_type"`
				SizeBytes *int64 `json:"size_bytes"`
				Revision  string `json:"revision"`
				Status    string `json:"status"`
			} `json:"entry"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil {
		return UploadReuseReceipt{}, errors.New("无法读取文件复用回执，请重试同一条记录")
	}
	entry := result.Data.Entry
	if !validEntryID(entry.ID) || entry.ParentID != declaration.ParentID || entry.Name == "" ||
		entry.EntryType != "FILE" || entry.SizeBytes == nil || *entry.SizeBytes != declaration.SizeBytes ||
		entry.Revision != "1" || entry.Status != "ACTIVE" {
		return UploadReuseReceipt{}, errors.New("文件复用回执与本机声明不一致，请重试同一条记录")
	}
	return UploadReuseReceipt{EntryID: entry.ID, Name: entry.Name}, nil
}
