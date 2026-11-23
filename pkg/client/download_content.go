package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DownloadContent 流式写入授权对象并校验字节数和 SHA-256。失败时由调用方丢弃目标。
func DownloadContent(ctx context.Context, grant DownloadGrant, destination io.Writer) error {
	if destination == nil {
		return errors.New("无效的下载目标")
	}
	hasher := sha256.New()
	if err := DownloadContentFrom(ctx, grant, 0, func(int64) (io.Writer, error) {
		return io.MultiWriter(destination, hasher), nil
	}); err != nil {
		return err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != grant.SHA256 {
		return errors.New("下载正文 SHA-256 校验失败，请重试")
	}
	return nil
}

// DownloadContentFrom reads the authorized object from offset. A full 200 restarts at zero.
// prepare selects the writer only after the response headers are validated; the caller verifies the entire file.
func DownloadContentFrom(ctx context.Context, grant DownloadGrant, offset int64, prepare func(int64) (io.Writer, error)) error {
	// 1. 用原签名链接发起存储 GET。续传只请求从已落盘偏移开始的剩余字节。
	if prepare == nil || grant.SizeBytes > maxJSONSafeInteger || !validStorageURL(grant.URL) ||
		offset < 0 || (offset != 0 && offset >= int64(grant.SizeBytes)) {
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
	if offset != 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	contentClient := serviceHTTPClient
	contentClient.Timeout = 5*time.Minute + 10*time.Second
	response, err := contentClient.Do(request)
	if err != nil {
		return errors.New("无法读取存储端的下载正文，请重试")
	}
	defer response.Body.Close()

	// 2. 回退到完整正文时从零写起。仅当 206 精确覆盖剩余范围时才允许追加。
	size := int64(grant.SizeBytes)
	ranges := response.Header.Values("Content-Range")
	switch {
	case response.StatusCode == http.StatusOK && len(ranges) == 0:
		offset = 0
	case offset > 0 && response.StatusCode == http.StatusPartialContent &&
		len(ranges) == 1 && ranges[0] == fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size):
	default:
		if offset > 0 {
			return errors.New("存储端返回的下载范围与断点不符，请重试")
		}
		return errors.New("存储端未返回完整文件正文，请重试")
	}
	for _, encoding := range response.Header.Values("Content-Encoding") {
		if encoding != "" && encoding != "identity" {
			return errors.New("存储端返回了非预期的正文编码")
		}
	}

	expectedSize := size - offset
	if response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return errors.New("下载正文大小与授权不符，请重试")
	}
	destination, err := prepare(offset)
	if err != nil {
		return err
	}
	if destination == nil {
		return errors.New("无效的下载目标")
	}

	// 3. 只写请求范围内的字节，并在返回成功前确认真实的正文末尾。
	written, err := io.Copy(destination, io.LimitReader(response.Body, expectedSize))
	if errors.Is(err, io.ErrUnexpectedEOF) || (err == nil && written < expectedSize) {
		return errors.New("下载正文被截断，请重试")
	}
	if err != nil {
		return errors.New("读取或写入下载正文失败，请重试")
	}

	var extraByte [1]byte
	switch _, err := io.ReadFull(response.Body, extraByte[:]); {
	case err == nil:
		return errors.New("下载正文超过授权大小，请重试")
	case !errors.Is(err, io.EOF):
		return errors.New("无法确认下载正文已结束，请重试")
	}
	return nil
}
