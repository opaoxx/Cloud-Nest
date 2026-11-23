package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DownloadGrant 保存一次授权返回的对象身份和短期存储地址。
type DownloadGrant struct {
	URL       string
	ExpiresAt time.Time
	ObjectID  string
	Revision  string
	SizeBytes uint64
	SHA256    string
}

// CreateDownloadGrant 为选中的文件修订申请下载授权。
func CreateDownloadGrant(ctx context.Context, address, accessToken, entryID, expectedRevision string) (DownloadGrant, error) {
	// 1. 使用列表中的条目 ID 和修订号，按下载用途提交授权请求。
	if !validEntryID(entryID) || !validEntryID(expectedRevision) {
		return DownloadGrant{}, errors.New("文件条目或修订号有误，请刷新目录后重新选择")
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return DownloadGrant{}, err
	}
	body, err := json.Marshal(struct {
		ExpectedRevision string `json:"expected_revision"`
		Purpose          string `json:"purpose"`
	}{ExpectedRevision: expectedRevision, Purpose: "download"})
	if err != nil {
		return DownloadGrant{}, errors.New("无法编码下载授权请求")
	}
	endpoint := base.JoinPath("api", "v1", "entries", entryID, "download")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return DownloadGrant{}, errors.New("无法构造下载授权请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")

	// 2. 区分访问令牌过期、修订变化和内容不可用，保留原修订供调用方判断。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return DownloadGrant{}, errors.New("无法连接服务以取得下载授权，请稍后重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return DownloadGrant{}, ErrAccessTokenExpired
		case response.StatusCode == http.StatusConflict && code == "REVISION_CONFLICT":
			return DownloadGrant{}, errors.New("文件修订已变化，请刷新目录后重新选择")
		case response.StatusCode == http.StatusConflict && code == "TARGET_UNAVAILABLE":
			return DownloadGrant{}, errors.New("文件内容当前不可用")
		case response.StatusCode == http.StatusNotFound:
			return DownloadGrant{}, errors.New("文件不存在或已不可访问，请刷新目录")
		default:
			return DownloadGrant{}, errors.New("服务端未交付下载授权")
		}
	}

	// 3. 校验回执的修订、对象、大小、摘要和链接，再保留签名地址的原始文本。
	var result struct {
		Data struct {
			URL         string    `json:"url"`
			ExpiresAt   time.Time `json:"expires_at"`
			ObjectID    string    `json:"object_id"`
			Revision    string    `json:"revision"`
			SizeBytes   *uint64   `json:"size_bytes"`
			SHA256      string    `json:"sha256"`
			Disposition string    `json:"disposition"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil {
		return DownloadGrant{}, errors.New("无法读取下载授权回执")
	}
	data := result.Data
	digest, digestErr := hex.DecodeString(data.SHA256)
	disposition, parameters, dispositionErr := mime.ParseMediaType(data.Disposition)
	if data.Revision != expectedRevision || !validEntryID(data.ObjectID) ||
		data.SizeBytes == nil || *data.SizeBytes > maxJSONSafeInteger ||
		digestErr != nil || len(digest) != 32 || data.SHA256 != strings.ToLower(data.SHA256) ||
		!validDownloadURL(data.URL) || dispositionErr != nil || disposition != "attachment" || parameters["filename"] == "" {
		return DownloadGrant{}, errors.New("服务返回的下载授权有误")
	}
	if !data.ExpiresAt.After(time.Now()) {
		return DownloadGrant{}, errors.New("下载授权链接已过期，请重新获取")
	}
	return DownloadGrant{
		URL: data.URL, ExpiresAt: data.ExpiresAt, ObjectID: data.ObjectID,
		Revision: data.Revision, SizeBytes: *data.SizeBytes, SHA256: data.SHA256,
	}, nil
}

func validDownloadURL(rawURL string) bool {
	if strings.ContainsFunc(rawURL, func(character rune) bool {
		return character <= 32 || character >= 127 || character == '#' || character == '\\'
	}) {
		return false
	}
	address, err := url.Parse(rawURL)
	return err == nil && (address.Scheme == "http" || address.Scheme == "https") &&
		address.Hostname() != "" && address.User == nil
}
