package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"
)

// DownloadContent 流式写入授权对象并校验字节数和 SHA-256。失败时由调用方丢弃目标。
func DownloadContent(ctx context.Context, grant DownloadGrant, destination io.Writer) error {
	// 1. 用原签名地址发起完整 GET，独立于业务接口的 Bearer 认证。
	if destination == nil || grant.SizeBytes > maxJSONSafeInteger || !validDownloadURL(grant.URL) {
		return errors.New("无效的下载目标")
	}
	if !grant.ExpiresAt.After(time.Now()) {
		return errors.New("下载授权链接已过期，请重新下载")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, grant.URL, nil)
	if err != nil {
		return errors.New("无法构造下载正文请求")
	}
	request.Header.Set("Accept-Encoding", "identity")
	contentClient := serviceHTTPClient
	contentClient.Timeout = 5*time.Minute + 10*time.Second
	response, err := contentClient.Do(request)
	if err != nil {
		return errors.New("无法读取存储端的下载正文，请重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Range") != "" {
		return errors.New("存储端未返回完整文件正文，请重试")
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return errors.New("存储端返回了非预期的正文编码")
	}

	// 2. 写入最多声明大小的字节，同时计算摘要。已知长度可提前发现不符。
	expectedSize := int64(grant.SizeBytes)
	if response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return errors.New("下载正文大小与授权不符，请重试")
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hasher), io.LimitReader(response.Body, expectedSize))
	if errors.Is(err, io.ErrUnexpectedEOF) || (err == nil && written < expectedSize) {
		return errors.New("下载正文被截断，请重试")
	}
	if err != nil {
		return errors.New("读取或写入下载正文失败，请重试")
	}

	// 3. 确认已读到真正的正文末尾，再比较完整的 SHA-256。
	var extraByte [1]byte
	switch _, err := io.ReadFull(response.Body, extraByte[:]); {
	case err == nil:
		return errors.New("下载正文超过授权大小，请重试")
	case !errors.Is(err, io.EOF):
		return errors.New("无法确认下载正文已结束，请重试")
	}
	if hex.EncodeToString(hasher.Sum(nil)) != grant.SHA256 {
		return errors.New("下载正文 SHA-256 校验失败，请重试")
	}
	return nil
}
