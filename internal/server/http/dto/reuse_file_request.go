package dto

import (
	"crypto/sha256"
	"encoding/json"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ReuseFileRequest 保留四个必填 JSON 字段的原值，便于定位类型错误。
type ReuseFileRequest struct {
	ParentID  json.RawMessage `json:"parent_id"`
	Name      json.RawMessage `json:"name"`
	SizeBytes json.RawMessage `json:"size_bytes"`
	SHA256    json.RawMessage `json:"sha256"`
}

// ReuseFileInput 保存秒传服务使用的结构化声明。
type ReuseFileInput struct {
	ParentID  uint64
	Name      string
	SizeBytes uint64
	SHA256    [sha256.Size]byte
}

// ParseReuseFileRequest 校验公开声明格式，保留名称原文供服务执行统一命名规则。
func ParseReuseFileRequest(request ReuseFileRequest) (*ReuseFileInput, *FieldError) {
	// 1. 检查目录 ID 的 JSON 类型和标准十进制写法。
	var parentText string
	parentError := json.Unmarshal(request.ParentID, &parentText)
	parentID, validParent := parsePathID(parentText)
	if parentError != nil || !validParent {
		return nil, &FieldError{Field: "parent_id", Message: "父目录 ID 须为 1～18446744073709551615 的标准十进制字符串"}
	}

	// 2. 读取名称与显式大小，零字节是合法的文件大小。
	var name string
	if !utf8.Valid(request.Name) || json.Unmarshal(request.Name, &name) != nil || name == "" {
		return nil, &FieldError{Field: "name", Message: "名称须为非空的有效 UTF-8 文本"}
	}
	var sizeBytes *uint64
	if json.Unmarshal(request.SizeBytes, &sizeBytes) != nil || sizeBytes == nil || *sizeBytes > maxJSONSafeInteger {
		return nil, &FieldError{Field: "size_bytes", Message: "文件大小须为 0～9007199254740991 的 JSON 整数"}
	}

	// 3. 整文件和分片使用相同的 SHA-256 十六进制编码规则。
	var digestText string
	decodeError := json.Unmarshal(request.SHA256, &digestText)
	digest, validDigest := domain.DecodeUploadPartSHA256(digestText)
	if decodeError != nil || !validDigest {
		return nil, &FieldError{Field: "sha256", Message: "SHA-256 须为 64 位十六进制文本"}
	}
	return &ReuseFileInput{ParentID: parentID, Name: name, SizeBytes: *sizeBytes, SHA256: digest}, nil
}
