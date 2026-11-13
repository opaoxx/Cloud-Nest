package dto

import (
	"regexp"
	"unicode/utf8"

	"github.com/google/uuid"
)

// loginUsernamePattern 与 users 的 ASCII 字符约束及 VARCHAR(64) 上限保持一致。
var loginUsernamePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// LoginRequest 描述登录输入，不包含数据库模型或服务端会话凭证。
type LoginRequest struct {
	// Username 是与数据库一致的小写 ASCII 登录名。
	Username string `json:"username"`
	// Password 是原始密码，不做去空白、大小写转换或其他修改。
	Password string `json:"password"`
	// InstallationID 是客户端生成并持久保存的标准小写 UUID v4。
	InstallationID string `json:"installation_id"`
	// DeviceName 是用于设备列表展示的名称。
	DeviceName string `json:"device_name"`
	// Platform 是 WEB、WINDOWS 或 ANDROID。
	Platform string `json:"platform"`
	// ClientType 是 web 或 native，用于后续选择凭证交付方式。
	ClientType string `json:"client_type"`
}

// Validate 检查字段格式，返回第一个可公开的错误；通过时返回 nil。
// 校验不修改请求，不查询账号，也不判断密码是否正确。
func (request LoginRequest) Validate() *FieldError {
	// 1. 检查用户名是否符合数据库的小写 ASCII 字母、数字、下划线及长度规则。
	if !loginUsernamePattern.MatchString(request.Username) {
		return &FieldError{Field: "username", Message: "用户名须为 1～64 个小写字母、数字或下划线"}
	}

	// 2. 检查密码字节长度，不改变原始密码。
	if !utf8.ValidString(request.Password) || len(request.Password) < 1 || len(request.Password) > 72 {
		return &FieldError{Field: "password", Message: "密码须为有效 UTF-8，长度为 1～72 字节"}
	}

	// 3. 检查标准安装 UUID v4 和设备名称的字符长度及控制字符。
	installationID, parseError := uuid.Parse(request.InstallationID)
	if parseError != nil || len(request.InstallationID) != 36 || installationID.Version() != 4 || installationID.Variant() != uuid.RFC4122 || installationID.String() != request.InstallationID {
		return &FieldError{Field: "installation_id", Message: "安装标识须为标准小写 UUID v4"}
	}
	if !validDeviceName(request.DeviceName) {
		return &FieldError{Field: "device_name", Message: "设备名称须为 1～100 个字符，不能全为空白或包含控制字符"}
	}

	// 4. 检查平台、客户端类型及其组合，不把客户端自报信息作为身份凭证。
	if request.Platform != "WEB" && request.Platform != "WINDOWS" && request.Platform != "ANDROID" {
		return &FieldError{Field: "platform", Message: "平台须为 WEB、WINDOWS 或 ANDROID"}
	}
	if request.ClientType != "web" && request.ClientType != "native" {
		return &FieldError{Field: "client_type", Message: "客户端类型须为 web 或 native"}
	}
	if (request.ClientType == "web") != (request.Platform == "WEB") {
		return &FieldError{Field: "platform", Message: "web 客户端须使用 WEB 平台，native 客户端须使用 WINDOWS 或 ANDROID 平台"}
	}
	return nil
}
