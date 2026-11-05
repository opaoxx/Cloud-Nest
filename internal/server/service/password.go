package service

import (
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"golang.org/x/crypto/bcrypt"
)

// bcryptHashPattern 只检查项目支持的标准外形，密码计算仍完全交给 bcrypt。
var bcryptHashPattern = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

// maxPasswordVerifyCost 是本项目的成本上限，当前初始化账号使用成本 12。
const maxPasswordVerifyCost = 14

// VerifyPassword 使用已有 bcrypt 摘要验证原始密码，不修改输入、不记录日志。
// 匹配为 true/nil，不匹配或无效候选密码为 false/nil，异常摘要为 false/固定内部错误。
// 方法同步执行，不承诺通过 context 中断 bcrypt 计算；账号状态及登录结果由后续业务层处理。
func VerifyPassword(passwordHash, rawPassword string) (bool, error) {
	// 1. 检查数据库摘要格式和本项目支持的计算成本。
	if !bcryptHashPattern.MatchString(passwordHash) {
		return false, domain.ErrInvalidPasswordHash
	}
	hashBytes := []byte(passwordHash)
	cost, err := bcrypt.Cost(hashBytes)
	if err != nil || cost > maxPasswordVerifyCost {
		return false, domain.ErrInvalidPasswordHash
	}

	// 2. 检查候选密码字节范围，保留原始内容。
	if !utf8.ValidString(rawPassword) || len(rawPassword) == 0 || len(rawPassword) > 72 {
		return false, nil
	}

	// 3. 调用 bcrypt 完成密码比对。
	err = bcrypt.CompareHashAndPassword(hashBytes, []byte(rawPassword))

	// 4. 区分不匹配与摘要异常，不附带敏感数据。
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, domain.ErrInvalidPasswordHash
	}
	return false, nil
}
