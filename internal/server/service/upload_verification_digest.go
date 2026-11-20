package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadMIMESampleBytes 是 Go 媒体类型识别使用的文件头长度上限。
const uploadMIMESampleBytes = 512

// ReadUploadVerificationDigest 读取原始对象流，返回实际大小、SHA-256 和识别出的 MIME。
// 调用方负责关闭对象流；大小不符、读取故障及取消均返回空结果。
func ReadUploadVerificationDigest(ctx context.Context, source io.Reader, declaredSize uint64) (*domain.UploadContentDigest, error) {
	// 1. 根据文件大小准备有上限的内容采样。
	sampleSize := declaredSize
	if declaredSize > maxTextPreviewBytes {
		sampleSize = uploadMIMESampleBytes
	}
	sample := make(uploadVerificationSample, 0, int(sampleSize))

	// 2. 复用原来的流式读取，在计算摘要时顺手采样。
	digest, err := CopyUploadContent(ctx, &sample, source, declaredSize)
	if err != nil {
		return nil, err
	}

	// 3. 大小核对通过后识别媒体类型，并随摘要交付。
	digest.DetectedMIMEType = detectUploadMIMEType(sample, digest.SizeBytes)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return digest, nil
}

// uploadVerificationSample 留存容量以内的文件前缀。
type uploadVerificationSample []byte

func (sample *uploadVerificationSample) Write(content []byte) (int, error) {
	// 1. 只保存剩余容量覆盖的字节，其余字节仍由复制链计算摘要。
	kept := min(len(content), cap(*sample)-len(*sample))
	*sample = append(*sample, content[:kept]...)
	return len(content), nil
}

func detectUploadMIMEType(sample []byte, sizeBytes uint64) string {
	// 1. 依据文件头识别首期支持的栅格图片。
	mediaType := http.DetectContentType(sample)
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mediaType
	case "text/plain; charset=utf-8":
		// 2. 纯文本须非空且完整可读，控制字符仅允许常见的换行与制表。
		if sizeBytes != 0 && uint64(len(sample)) == sizeBytes && utf8.Valid(sample) &&
			!bytes.ContainsFunc(sample, func(character rune) bool {
				return unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t'
			}) {
			return "text/plain"
		}
	}
	return ""
}
