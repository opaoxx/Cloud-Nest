package domain

import "errors"

// ErrInvalidCredentials 统一表示账号不存在或密码错误，不区分具体失败原因。
var ErrInvalidCredentials = errors.New("用户名或密码错误")

// ErrAccountDisabled 仅在密码匹配后表示账号已被禁用。
var ErrAccountDisabled = errors.New("账号已被禁用")

// ErrInvalidAccountData 表示查询返回了缺失或不符合预期的账号数据，属于内部故障。
var ErrInvalidAccountData = errors.New("账号数据无效")

// VerifiedAccount 是凭证和状态检查通过的最小身份，尚未创建登录会话。
type VerifiedAccount struct {
	// ID 是后续设备、会话流程使用的用户标识，不包含密码摘要。
	ID uint64
}
