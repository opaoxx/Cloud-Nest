package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const uploadPartChecksumHeader = "x-amz-checksum-sha256"
const maxUploadPartGrantBatch = 20

var uploadPartETagPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// ErrUploadPartURLExpired identifies a signed URL that expired before storage accepted the part.
var ErrUploadPartURLExpired = errors.New("分片签名已过期")

// ErrMultipartUploadInitializing indicates that the saved task is waiting for storage initialization.
var ErrMultipartUploadInitializing = errors.New("服务端仍在准备上传，请稍后继续")

// UploadPartGrant holds a storage PUT authorization. Credentials stay inside this package.
type UploadPartGrant struct {
	url       string
	expiresAt time.Time
	number    int
	sizeBytes int64
	digest    [sha256.Size]byte
}

// UploadPartReceipt identifies the exact part accepted by storage.
type UploadPartReceipt struct {
	Number    int
	SizeBytes int64
	SHA256    string
	ETag      string
}

// UploadPartDeclaration binds a planned part number to its bytes and digest.
type UploadPartDeclaration struct {
	Number    int
	SizeBytes int64
	Digest    [sha256.Size]byte
}

// CheckMultipartUploadReady checks the current state of the task saved in the queue.
func CheckMultipartUploadReady(ctx context.Context, address, accessToken, taskID, parentID string, sizeBytes int64) error {
	task, err := readUploadTask(ctx, address, accessToken, taskID, "MULTIPART", sizeBytes)
	if err != nil {
		return err
	}
	if task.ParentID != parentID {
		return errors.New("远端任务的目标目录与本机记录不一致")
	}
	if task.ExpiresAt.IsZero() {
		return errors.New("远端任务的有效期无效")
	}
	if !task.ExpiresAt.After(time.Now()) || task.Status == "EXPIRED" || task.Status == "EXPIRING" {
		return errors.New("原分片上传任务已到期，无法继续")
	}
	switch task.Status {
	case "UPLOADING":
		return nil
	case "CREATING":
		return ErrMultipartUploadInitializing
	default:
		return errors.New("远端任务当前不接收分片")
	}
}

// IssueUploadPartURL requests a single signed PUT using the calculated part digest.
func IssueUploadPartURL(ctx context.Context, address, accessToken, taskID string, number int, sizeBytes int64, digest [sha256.Size]byte) (UploadPartGrant, error) {
	batch, err := IssueUploadPartURLs(ctx, address, accessToken, taskID,
		[]UploadPartDeclaration{{Number: number, SizeBytes: sizeBytes, Digest: digest}})
	if err != nil {
		return UploadPartGrant{}, err
	}
	return batch[0], nil
}

// RefreshUploadPartURL replaces the expired signature for the same part and storage session.
func RefreshUploadPartURL(ctx context.Context, address, accessToken, taskID string, expired UploadPartGrant) (UploadPartGrant, error) {
	renewed, err := IssueUploadPartURL(ctx, address, accessToken, taskID, expired.number, expired.sizeBytes, expired.digest)
	if err != nil {
		return UploadPartGrant{}, err
	}
	before, oldError := url.Parse(expired.url)
	after, newError := url.Parse(renewed.url)
	if oldError != nil || newError != nil || before.Scheme != after.Scheme || before.Host != after.Host ||
		before.EscapedPath() != after.EscapedPath() || before.Query().Get("uploadId") != after.Query().Get("uploadId") {
		return UploadPartGrant{}, errors.New("分片存储会话已变化，请重新核对任务")
	}
	if renewed.url == expired.url || !renewed.expiresAt.After(time.Now()) {
		return UploadPartGrant{}, errors.New("未取得新的有效分片签名，请稍后重试")
	}
	return renewed, nil
}

// IssueUploadPartURLs requests a batch of storage PUT authorizations in declaration order.
func IssueUploadPartURLs(ctx context.Context, address, accessToken, taskID string, parts []UploadPartDeclaration) ([]UploadPartGrant, error) {
	// 1. 向业务 API 申报不同的片号和各自的十六进制摘要。
	if !validEntryID(taskID) || len(parts) == 0 || len(parts) > maxUploadPartGrantBatch {
		return nil, errors.New("无效的分片签名批次")
	}
	type partDeclaration struct {
		Number int    `json:"part_number"`
		SHA256 string `json:"sha256"`
	}
	declarations := make([]partDeclaration, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, part := range parts {
		if part.Number < 1 || part.Number > maxUploadParts || part.SizeBytes < 1 || part.SizeBytes > maxJSONSafeInteger || seen[part.Number] {
			return nil, errors.New("无效或重复的分片声明")
		}
		seen[part.Number] = true
		declarations = append(declarations, partDeclaration{Number: part.Number, SHA256: hex.EncodeToString(part.Digest[:])})
	}
	base, err := parseServiceAddress(address)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Parts []partDeclaration `json:"parts"`
	}{Parts: declarations})
	if err != nil {
		return nil, errors.New("无法编码分片签名请求")
	}
	endpoint := base.JoinPath("api", "v1", "uploads", taskID, "part-urls")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("无法构造分片签名请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return nil, errors.New("未取得分片签名，请重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		code := authErrorCode(response.Body)
		switch {
		case response.StatusCode == http.StatusUnauthorized && code == "TOKEN_EXPIRED":
			return nil, ErrAccessTokenExpired
		case code == "TASK_NOT_READY" || code == "UPLOAD_BUSY":
			return nil, errors.New("远端任务暂不可签发分片，请稍后重试")
		default:
			return nil, errors.New("服务端未交付分片签名")
		}
	}

	// 2. 核对回执的片号、长度、校验头和期限，URL 原样保留。
	var result struct {
		Data struct {
			ExpiresAt time.Time `json:"expires_at"`
			Parts     []struct {
				Number    int               `json:"part_number"`
				SizeBytes int64             `json:"size_bytes"`
				URL       string            `json:"url"`
				Headers   map[string]string `json:"required_headers"`
				ExpiresAt time.Time         `json:"url_expires_at"`
			} `json:"parts"`
		} `json:"data"`
	}
	if readBoundedJSON(response.Body, 256*1024, &result) != nil || len(result.Data.Parts) != len(parts) {
		return nil, errors.New("服务返回的分片签名格式有误")
	}
	grants := make([]UploadPartGrant, 0, len(parts))
	for index, part := range result.Data.Parts {
		original := parts[index]
		if part.Number != original.Number || part.SizeBytes != original.SizeBytes || !validStorageURL(part.URL) ||
			len(part.Headers) != 1 || part.Headers[uploadPartChecksumHeader] != base64.StdEncoding.EncodeToString(original.Digest[:]) ||
			part.ExpiresAt.IsZero() || part.ExpiresAt.After(result.Data.ExpiresAt) {
			return nil, errors.New("分片签名与本机声明不一致")
		}
		signedURL, err := url.Parse(part.URL)
		if err != nil || signedURL.Query().Get("partNumber") != strconv.Itoa(original.Number) || signedURL.Query().Get("uploadId") == "" {
			return nil, errors.New("分片签名的存储会话或片号无效")
		}
		grants = append(grants, UploadPartGrant{
			url: part.URL, expiresAt: part.ExpiresAt, number: original.Number, sizeBytes: original.SizeBytes, digest: original.Digest,
		})
	}
	return grants, nil
}

// SendUploadPart streams a part to storage and returns its normalized ETag.
func SendUploadPart(ctx context.Context, grant UploadPartGrant, source io.Reader) (UploadPartReceipt, error) {
	// 1. 对存储端只发送签名要求的长度与校验头，不携带业务令牌。
	if source == nil {
		return UploadPartReceipt{}, errors.New("分片正文不可用，请重试")
	}
	if !grant.expiresAt.After(time.Now()) {
		return UploadPartReceipt{}, ErrUploadPartURLExpired
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, grant.url, io.LimitReader(source, grant.sizeBytes))
	if err != nil {
		return UploadPartReceipt{}, errors.New("无法构造分片正文请求")
	}
	request.ContentLength = grant.sizeBytes
	request.Header.Set(uploadPartChecksumHeader, base64.StdEncoding.EncodeToString(grant.digest[:]))
	storageClient := serviceHTTPClient
	storageClient.Timeout = 5*time.Minute + 10*time.Second
	response, err := storageClient.Do(request)
	if err != nil {
		return UploadPartReceipt{}, errors.New("未确认存储端是否收到分片，请重试同一片")
	}
	defer response.Body.Close()

	// 2. 存储的成功状态和有效 ETag 共同构成可保存的回执。
	if response.StatusCode != http.StatusOK {
		if isExpiredUploadPartResponse(response.StatusCode, response.Body) {
			return UploadPartReceipt{}, ErrUploadPartURLExpired
		}
		return UploadPartReceipt{}, errors.New("存储端未接受分片，请重新检查源文件后重试")
	}
	tags := response.Header.Values("ETag")
	if len(tags) != 1 {
		return UploadPartReceipt{}, errors.New("存储端未返回有效的分片 ETag")
	}
	tag := tags[0]
	if len(tag) >= 2 && strings.HasPrefix(tag, "\"") && strings.HasSuffix(tag, "\"") {
		tag = tag[1 : len(tag)-1]
	}
	if !uploadPartETagPattern.MatchString(tag) {
		return UploadPartReceipt{}, errors.New("存储端未返回有效的分片 ETag")
	}
	return UploadPartReceipt{Number: grant.number, SizeBytes: grant.sizeBytes, SHA256: hex.EncodeToString(grant.digest[:]), ETag: tag}, nil
}

func isExpiredUploadPartResponse(status int, body io.Reader) bool {
	if status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	const maxErrorBytes = 8192
	reply, err := io.ReadAll(io.LimitReader(body, maxErrorBytes+1))
	if err != nil || len(reply) > maxErrorBytes {
		return false
	}
	var rejection struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	if xml.Unmarshal(reply, &rejection) != nil {
		return false
	}
	// MinIO 的过期响应与一般权限错误共享 AccessDenied，须同时核对原因。
	return rejection.Code == "RequestExpired" ||
		(status == http.StatusForbidden && rejection.Code == "AccessDenied" && rejection.Message == "Request has expired")
}
