package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// RenameFile changes the name of the visible file revision and returns the confirmed entry.
func RenameFile(ctx context.Context, address, accessToken, entryID, revision, parentID, name, key string) (DirectoryEntry, error) {
	if strings.TrimSpace(name) == "" {
		return DirectoryEntry{}, errors.New("请输入文件名称")
	}
	return editFile(ctx, address, accessToken, entryID, revision, parentID, key, "", name)
}

// MoveFile changes the parent of the visible file revision and returns the confirmed entry.
func MoveFile(ctx context.Context, address, accessToken, entryID, revision, parentID, targetParentID, key string) (DirectoryEntry, error) {
	if !validEntryID(targetParentID) || targetParentID == parentID {
		return DirectoryEntry{}, errors.New("请选择其他文件夹")
	}
	return editFile(ctx, address, accessToken, entryID, revision, parentID, key, targetParentID, "")
}

func editFile(ctx context.Context, address, accessToken, entryID, revision, parentID, key, targetParentID, name string) (DirectoryEntry, error) {
	// 1. Send the observed revision and the same key on retries of this change.
	if !validEntryID(entryID) || !validEntryID(revision) || !validEntryID(parentID) || key == "" {
		return DirectoryEntry{}, errors.New("文件信息有误，请刷新目录后重试")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return DirectoryEntry{}, err
	}
	body, err := json.Marshal(struct {
		Revision       string `json:"expected_revision"`
		Name           string `json:"name,omitempty"`
		TargetParentID string `json:"target_parent_id,omitempty"`
	}{Revision: revision, Name: name, TargetParentID: targetParentID})
	if err != nil {
		return DirectoryEntry{}, err
	}
	method := http.MethodPatch
	endpoint := base.JoinPath("api", "v1", "entries", entryID)
	if targetParentID != "" {
		method = http.MethodPost
		endpoint = endpoint.JoinPath("move")
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return DirectoryEntry{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)

	// 2. Give actionable messages for conflicts and retain the shared session retry behavior.
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return DirectoryEntry{}, errors.New("无法确认文件操作结果，请重试或刷新目录核对")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch code := authErrorCode(response.Body); {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return DirectoryEntry{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusUnauthorized && code == "SESSION_REVOKED":
			return DirectoryEntry{}, ErrFileSessionRevoked
		case code == "REVISION_CONFLICT":
			return DirectoryEntry{}, errors.New("文件已发生变化，请刷新目录后重试")
		case code == "NAME_CONFLICT":
			return DirectoryEntry{}, errors.New("该目录中已有同名文件或文件夹")
		case code == "INVALID_NAME":
			return DirectoryEntry{}, errors.New("该文件名不可用，请更换名称")
		case response.StatusCode == http.StatusNotFound:
			return DirectoryEntry{}, errors.New("文件或目标文件夹已不可访问，请刷新目录")
		default:
			return DirectoryEntry{}, errors.New("服务端未确认文件操作，请刷新目录核对")
		}
	}

	// 3. Both endpoints return entry details. Verify the file and the expected parent.
	var receipt struct {
		Data struct {
			Entry struct {
				DirectoryEntry
				Kind   string `json:"kind"`
				Status string `json:"status"`
			} `json:"entry"`
		} `json:"data"`
	}
	if err := readSmallJSON(response.Body, &receipt); err != nil {
		return DirectoryEntry{}, errors.New("无法读取文件操作结果，请刷新目录核对")
	}
	entry := receipt.Data.Entry
	expectedParent := parentID
	if targetParentID != "" {
		expectedParent = targetParentID
	}
	if entry.ID != entryID || entry.ParentID != expectedParent || entry.Status != "ACTIVE" ||
		entry.Kind == "folder" || entry.Kind == "" || entry.Name == "" ||
		!validEntryID(entry.Revision) || entry.SizeBytes > maxJSONSafeInteger {
		return DirectoryEntry{}, errors.New("服务返回的文件操作结果有误，请刷新目录核对")
	}
	entry.EntryType = "FILE"
	return entry.DirectoryEntry, nil
}
