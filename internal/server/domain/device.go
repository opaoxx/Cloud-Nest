package domain

import (
	"errors"
	"time"
)

// ErrDeviceNotFound 表示该用户与安装标识组合未找到设备，不表示数据库故障。
var ErrDeviceNotFound = errors.New("设备不存在")

// ErrDeviceAlreadyExists 表示设备插入触发唯一键冲突，不允许静默覆盖已有设备。
var ErrDeviceAlreadyExists = errors.New("该用户的安装设备已存在")

// LoginDevice 保存登录设备所需的信息，不作为 HTTP 响应使用。
type LoginDevice struct {
	// ID 是设备记录的无符号主键。
	ID uint64
	// UserID 是设备所属用户，不能用请求正文替代已验证的用户身份。
	UserID uint64
	// InstallationID 是该用户下的客户端安装 UUID。
	InstallationID string
	// Name 是已有设备的展示名称。
	Name string
	// Platform 是已有设备的 WEB、WINDOWS 或 ANDROID 平台。
	Platform string
	// RevokedAt 为 nil 表示未撤销，否则保留数据库中的撤销时间。
	RevokedAt *time.Time
}
