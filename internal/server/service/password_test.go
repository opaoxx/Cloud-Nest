package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"golang.org/x/crypto/bcrypt"
)

func TestVerifyPasswordMatchesAndBoundaries(t *testing.T) {
	// 1. 生成仅供测试的低成本摘要，验证候选密码原文和 UTF-8 字节边界。
	for _, testCase := range []struct {
		name, storedPassword, rawPassword string
		wantMatch                         bool
	}{
		{name: "root matches", storedPassword: "root", rawPassword: "root", wantMatch: true},
		{name: "wrong password", storedPassword: "root", rawPassword: "private-wrong-password"},
		{name: "case sensitive", storedPassword: "root", rawPassword: "ROOT"},
		{name: "spaces not removed", storedPassword: "root", rawPassword: " root "},
		{name: "stored spaces preserved", storedPassword: " root ", rawPassword: " root ", wantMatch: true},
		{name: "trimmed candidate differs", storedPassword: " root ", rawPassword: "root"},
		{name: "only spaces valid", storedPassword: " ", rawPassword: " ", wantMatch: true},
		{name: "unicode matches", storedPassword: "学习🎮", rawPassword: "学习🎮", wantMatch: true},
		{name: "unicode not normalized", storedPassword: "café", rawPassword: "cafe\u0301"},
		{name: "minimum length", storedPassword: "a", rawPassword: "a", wantMatch: true},
		{name: "exact 72 bytes", storedPassword: strings.Repeat("a", 72), rawPassword: strings.Repeat("a", 72), wantMatch: true},
		{name: "longer candidate never accepted as prefix", storedPassword: strings.Repeat("a", 72), rawPassword: strings.Repeat("a", 72) + "x"},
		{name: "72 utf8 bytes", storedPassword: strings.Repeat("中", 24), rawPassword: strings.Repeat("中", 24), wantMatch: true},
		{name: "over utf8 byte limit", storedPassword: strings.Repeat("中", 24), rawPassword: strings.Repeat("中", 25)},
		{name: "empty candidate rejected", storedPassword: "root"},
		{name: "invalid utf8 rejected", storedPassword: "root", rawPassword: "\xff"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hash, err := bcrypt.GenerateFromPassword([]byte(testCase.storedPassword), bcrypt.MinCost)
			if err != nil {
				t.Fatal("生成临时测试摘要失败")
			}
			matched, err := VerifyPassword(string(hash), testCase.rawPassword)
			if err != nil || matched != testCase.wantMatch {
				t.Fatalf("比对结果错误 matched=%v error=%v", matched, err)
			}
		})
	}
}

func TestVerifyPasswordHashPolicy(t *testing.T) {
	// 1. 修改临时摘要的外形，异常应在昂贵计算前返回固定内部错误。
	hashBytes, err := bcrypt.GenerateFromPassword([]byte("root"), bcrypt.MinCost)
	if err != nil {
		t.Fatal("生成临时测试摘要失败")
	}
	hash := string(hashBytes)
	for _, testCase := range []struct {
		name, hash string
	}{
		{name: "empty"},
		{name: "plaintext", hash: "private-plaintext-value"},
		{name: "truncated", hash: hash[:59]},
		{name: "appended data", hash: hash + "x"},
		{name: "prefix whitespace", hash: " " + hash},
		{name: "wrong major version", hash: "$3a$" + hash[4:]},
		{name: "unsupported minor version", hash: "$2x$" + hash[4:]},
		{name: "unknown minor version", hash: "$2z$" + hash[4:]},
		{name: "legacy format not supported", hash: "$2$" + hash[4:]},
		{name: "cost below library minimum", hash: hash[:4] + "03" + hash[6:]},
		{name: "cost above project maximum", hash: hash[:4] + "15" + hash[6:]},
		{name: "library max cost not accepted", hash: hash[:4] + "31" + hash[6:]},
		{name: "cost above library maximum", hash: hash[:4] + "32" + hash[6:]},
		{name: "cost non numeric", hash: hash[:4] + "xx" + hash[6:]},
		{name: "invalid salt alphabet", hash: hash[:7] + "#" + hash[8:]},
		{name: "invalid digest alphabet", hash: hash[:59] + "#"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			matched, err := VerifyPassword(testCase.hash, "private-candidate")
			if matched || !errors.Is(err, domain.ErrInvalidPasswordHash) || err != domain.ErrInvalidPasswordHash {
				t.Fatal("异常摘要没有返回固定内部错误")
			}
			if strings.Contains(err.Error(), "private-") || (testCase.hash != "" && strings.Contains(err.Error(), testCase.hash)) {
				t.Fatal("错误泄漏摘要或候选密码")
			}
		})
	}

	// 2. 接受标准 2a/2b/2y 格式；成本 14 的策略边界无需执行高成本计算即可检查。
	for _, prefix := range []string{"$2a$", "$2b$", "$2y$"} {
		matched, err := VerifyPassword(prefix+hash[4:], "root")
		if err != nil || !matched {
			t.Fatal("支持的摘要格式未通过比对")
		}
	}
	if matched, err := VerifyPassword(hash[:4]+"14"+hash[6:], ""); matched || err != nil {
		t.Fatal("成本 14 的摘要被当作不支持")
	}
}

func TestVerifyPasswordUsesStoredSalt(t *testing.T) {
	// 1. 同一个密码生成两个随机盐摘要，不能靠重新生成摘要后比较字符串来验证。
	firstHash, err := bcrypt.GenerateFromPassword([]byte("private-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal("生成第一个测试摘要失败")
	}
	secondHash, err := bcrypt.GenerateFromPassword([]byte("private-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal("生成第二个测试摘要失败")
	}
	if string(firstHash) == string(secondHash) {
		t.Fatal("两个临时摘要未使用不同随机盐")
	}
	for _, hash := range []string{string(firstHash), string(secondHash)} {
		if matched, err := VerifyPassword(hash, "private-password"); err != nil || !matched {
			t.Fatal("没有使用已有摘要的信息验证密码")
		}
	}
}

func TestVerifyPasswordIntegration(t *testing.T) {
	// 1. 只读查询现有开发账号，衔接第 019 步，不创建账号、会话或令牌。
	if os.Getenv("CLOUDNEST_PASSWORD_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_PASSWORD_TEST=1 后只读验证现有 root 密码")
	}
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connectionPool, err := repository.OpenMySQL(ctx, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer connectionPool.Close()
	store, err := repository.NewStore(connectionPool)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.FindLoginAccountByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}

	// 2. 正确开发密码匹配，错误和增加空格的候选不匹配；日志不打印摘要。
	for _, testCase := range []struct {
		password  string
		wantMatch bool
	}{
		{password: "root", wantMatch: true},
		{password: "private-wrong-password"},
		{password: " root "},
	} {
		matched, err := VerifyPassword(account.PasswordHash, testCase.password)
		if err != nil || matched != testCase.wantMatch {
			t.Fatal("现有开发账号的密码比对结果不符合预期")
		}
	}
}
