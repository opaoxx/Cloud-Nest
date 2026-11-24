package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ErrFileSessionRevoked 指示文件操作所用的服务端会话已撤销。
var ErrFileSessionRevoked = errors.New("当前会话已撤销，请重新登录")

// TrashFile 按所见修订移入回收站，只有服务确认同一文件后才返回成功。
func TrashFile(ctx context.Context, address, accessToken, entryID, revision, key string) error {
	// 1. 用原条目、修订和幂等键调用现有回收站入口。
	if !validEntryID(entryID) || !validEntryID(revision) || key == "" {
		return errors.New("文件信息有误，请刷新目录后重试")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Revision string `json:"expected_revision"`
	}{Revision: revision})
	if err != nil {
		return err
	}
	endpoint := base.JoinPath("api", "v1", "entries", entryID, "trash")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)

	// 2. 按服务返回的业务状态处理会话、修订和不存在的文件。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return errors.New("无法确认回收结果，请刷新目录核对")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch code := authErrorCode(response.Body); {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return ErrAccessTokenExpired
		case response.StatusCode == http.StatusUnauthorized && code == "SESSION_REVOKED":
			return ErrFileSessionRevoked
		case response.StatusCode == http.StatusConflict && code == "REVISION_CONFLICT":
			return errors.New("文件已发生变化，请刷新目录后重试")
		case response.StatusCode == http.StatusNotFound:
			return errors.New("文件不存在或已不可访问，请刷新目录")
		default:
			return errors.New("服务端未确认回收结果，请刷新目录核对")
		}
	}

	// 3. 验证稳定回收快照属于原文件。
	var result struct {
		Data struct {
			Entry struct {
				ID        string    `json:"id"`
				EntryType string    `json:"entry_type"`
				Revision  string    `json:"revision"`
				Status    string    `json:"status"`
				DeletedAt time.Time `json:"deleted_at"`
			} `json:"entry"`
		} `json:"data"`
	}
	if err := readSmallJSON(response.Body, &result); err != nil {
		return errors.New("无法读取回收结果，请刷新目录核对")
	}
	entry := result.Data.Entry
	if entry.ID != entryID || entry.EntryType != "FILE" || entry.Status != "TRASHED" ||
		!validEntryID(entry.Revision) || entry.DeletedAt.IsZero() {
		return errors.New("服务返回的回收结果有误，请刷新目录核对")
	}
	return nil
}
