package dto

import (
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// maxJSONSafeInteger 是 JavaScript Number 可安全表示的最大整数。
const maxJSONSafeInteger uint64 = 1<<53 - 1

// UserSummary 保存供客户端展示的最小用户信息。
type UserSummary struct {
	// ID 是完整 uint64 用户主键的十进制字符串。
	ID string `json:"id"`
	// Username 是用户登录名。
	Username string `json:"username"`
	// DisplayName 是按原文保留的展示名称。
	DisplayName string `json:"display_name"`
}

// UserProfileData 是登录资料与当前用户响应可复用的数据字段。
type UserProfileData struct {
	// User 保存用户标识和展示信息。
	User UserSummary `json:"user"`
	// RootEntryID 是真实根目录主键的十进制字符串。
	RootEntryID string `json:"root_entry_id"`
	// QuotaBytes 是 JSON 安全整数范围内的逻辑配额字节数。
	QuotaBytes uint64 `json:"quota_bytes"`
	// UsedBytes 是已用逻辑存储字节数，零值也明确返回。
	UsedBytes uint64 `json:"used_bytes"`
	// ReservedBytes 是上传预占字节数，零值也明确返回。
	ReservedBytes uint64 `json:"reserved_bytes"`
}

// NewUserProfileData 将服务返回的资料映射为响应数据，失败返回 nil。
// 账号状态、根目录归属及配额一致性由资料服务与 Repository 检查。
func NewUserProfileData(profile *domain.UserProfile) (*UserProfileData, error) {
	// 1. 检查资料及两个非零 ID。
	if profile == nil || profile.ID == 0 || profile.RootEntryID == 0 {
		return nil, domain.ErrInvalidAccountData
	}

	// 2. 检查字节数的 JSON 安全整数范围。
	if profile.QuotaBytes > maxJSONSafeInteger || profile.UsedBytes > maxJSONSafeInteger ||
		profile.ReservedBytes > maxJSONSafeInteger {
		return nil, fmt.Errorf("用户资料字节数超出 JSON 安全整数范围")
	}

	// 3. 转换 ID，映射用户与配额字段。
	return &UserProfileData{
		User: UserSummary{
			ID:          strconv.FormatUint(profile.ID, 10),
			Username:    profile.Username,
			DisplayName: profile.DisplayName,
		},
		RootEntryID:   strconv.FormatUint(profile.RootEntryID, 10),
		QuotaBytes:    profile.QuotaBytes,
		UsedBytes:     profile.UsedBytes,
		ReservedBytes: profile.ReservedBytes,
	}, nil
}
