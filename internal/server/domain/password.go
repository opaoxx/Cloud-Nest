package domain

import "errors"

// ErrInvalidPasswordHash 表示数据库摘要格式或成本不受当前项目支持，不属于普通密码不匹配。
// 此错误只在服务内部使用，不附带原始摘要、候选密码或解析器细节。
var ErrInvalidPasswordHash = errors.New("账号密码摘要无效或不受支持")
