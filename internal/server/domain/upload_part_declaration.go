package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

// MaxUploadPartSigningInputs 是一次分片签发请求可声明的最大项数。
const MaxUploadPartSigningInputs = 20

// UploadPartDeclaration 保存客户端声明的编号和分片 SHA-256 十六进制文本。
type UploadPartDeclaration struct {
	PartNumber uint32
	SHA256     string
}

// UploadPartDeclarationError 携带固定的公开字段名及提示，供调用方定位批次或单项错误。
type UploadPartDeclarationError struct {
	Field   string
	Message string
}

func (inputError *UploadPartDeclarationError) Error() string {
	return inputError.Message
}

// DecodeUploadPartSHA256 将恰好 64 位的十六进制文本解码为固定长度摘要。
func DecodeUploadPartSHA256(text string) ([sha256.Size]byte, bool) {
	// 1. 先检查长度，再验证每个字符并解码。
	var digest [sha256.Size]byte
	if len(text) != hex.EncodedLen(len(digest)) {
		return digest, false
	}
	if _, err := hex.Decode(digest[:], []byte(text)); err != nil {
		return [sha256.Size]byte{}, false
	}
	return digest, true
}
