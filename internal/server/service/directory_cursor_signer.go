package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	// directoryCursorKeyPurpose 标记本组件派生密钥的固定用途。
	directoryCursorKeyPurpose = "cloudnest/directory-cursor/v1"
	// directoryCursorTimeLayout 使用 UTC 固定六位小数保留数据库微秒位置。
	directoryCursorTimeLayout = "2006-01-02T15:04:05.000000Z"
	// directoryCursorEmptyFilters 固定当前无搜索、无分类筛选的规范表示。
	directoryCursorEmptyFilters = `{"q":"","kind":""}`
)

// directoryCursorPayload 定义版本 1 的普通目录默认排序游标。
type directoryCursorPayload struct {
	// Version 允许后续校验器识别载荷格式。
	Version uint8 `json:"version"`
	// UserID 绑定已经认证的用户，保留完整无符号范围。
	UserID string `json:"user_id"`
	// ParentID 绑定当前目录。
	ParentID string `json:"parent_id"`
	// Scope 固定为普通目录查询。
	Scope string `json:"scope"`
	// Sort 固定为修改时间、ID 降序。
	Sort string `json:"sort"`
	// FilterHash 绑定当前空搜索及空分类筛选。
	FilterHash string `json:"filter_hash"`
	// ModifiedAt 是 UTC 微秒时间文本。
	ModifiedAt string `json:"modified_at"`
	// EntryID 是上一页末项的完整目录项 ID。
	EntryID string `json:"entry_id"`
}

// DirectoryCursorSigner 使用独立用途的 HMAC 密钥生成目录分页游标。
type DirectoryCursorSigner struct {
	// signingKey 是构造时派生并由本组件持有的 32 字节游标密钥。
	signingKey []byte
}

// NewDirectoryCursorSigner 接收至少 32 字节主密钥，派生目录游标专用密钥。
// 后续装配可传入已有持久化签名配置，调用方的原始字节由调用方管理。
func NewDirectoryCursorSigner(masterKey []byte) (*DirectoryCursorSigner, error) {
	// 1. 核对主密钥长度。
	if len(masterKey) < sha256.Size {
		return nil, fmt.Errorf("目录游标签名需要至少 32 字节主密钥")
	}

	// 2. 用固定用途标签派生独立密钥，保存组件自己的结果。
	keyDerivation := hmac.New(sha256.New, masterKey)
	keyDerivation.Write([]byte(directoryCursorKeyPurpose))
	return &DirectoryCursorSigner{signingKey: keyDerivation.Sum(nil)}, nil
}

// Sign 为已验证的目录查询生成版本 1 游标，错误时返回空文本。
// userID、parentID 和 position 来自当前目录查询，使用 directory/modified_desc 和空筛选摘要。
func (signer *DirectoryCursorSigner) Sign(userID, parentID uint64, position domain.FileEntryPosition) (string, error) {
	// 1. 检查签名器和目录分页位置。
	if signer == nil || len(signer.signingKey) != sha256.Size {
		return "", fmt.Errorf("目录游标签名器尚未初始化")
	}
	if userID == 0 || parentID == 0 || !position.IsValid() {
		return "", domain.ErrInvalidDirectoryCursor
	}

	// 2. 规范时间并编码带范围信息的游标载荷。
	filterHash := sha256.Sum256([]byte(directoryCursorEmptyFilters))
	payload := directoryCursorPayload{
		Version:    1,
		UserID:     strconv.FormatUint(userID, 10),
		ParentID:   strconv.FormatUint(parentID, 10),
		Scope:      "directory",
		Sort:       "modified_desc",
		FilterHash: hex.EncodeToString(filterHash[:]),
		ModifiedAt: position.ModifiedAt.UTC().Format(directoryCursorTimeLayout),
		EntryID:    strconv.FormatUint(position.ID, 10),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("编码目录分页游标失败: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 3. 使用游标密钥计算 HMAC，组装 Base64URL 文本。
	messageMAC := hmac.New(sha256.New, signer.signingKey)
	messageMAC.Write([]byte(encodedPayload))
	encodedMAC := base64.RawURLEncoding.EncodeToString(messageMAC.Sum(nil))
	return encodedPayload + "." + encodedMAC, nil
}
