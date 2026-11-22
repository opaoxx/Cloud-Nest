package dto

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// parseRequiredUploadSize 读取必填的 JSON 安全整数，保留显式声明的零字节。
func parseRequiredUploadSize(rawSize json.RawMessage) (uint64, *FieldError) {
	var sizeBytes *uint64
	if json.Unmarshal(rawSize, &sizeBytes) != nil || sizeBytes == nil || *sizeBytes > maxJSONSafeInteger {
		return 0, &FieldError{Field: "size_bytes", Message: "文件大小须为 0～9007199254740991 的 JSON 整数"}
	}
	return *sizeBytes, nil
}

// parseRequiredUploadSHA256 将必填的整文件摘要转换为与十六进制大小写无关的字节。
func parseRequiredUploadSHA256(rawDigest json.RawMessage) ([sha256.Size]byte, *FieldError) {
	var digestText string
	decodeError := json.Unmarshal(rawDigest, &digestText)
	digest, validDigest := domain.DecodeUploadPartSHA256(digestText)
	if decodeError != nil || !validDigest {
		return [sha256.Size]byte{}, &FieldError{Field: "sha256", Message: "SHA-256 须为 64 位十六进制文本"}
	}
	return digest, nil
}
