package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestLoadOrCreateJWTConfigPersistence(t *testing.T) {
	clearJWTEnvironment(t)
	secretPath := filepath.Join(t.TempDir(), "CloudNest", "jwt-secret")
	first, err := LoadOrCreateJWTConfig(secretPath)
	if err != nil || len(first.SigningKey) != 32 {
		t.Fatalf("首次初始化失败: %v", err)
	}
	content, err := os.ReadFile(secretPath)
	if err != nil || len(content) != 43 || base64.RawURLEncoding.EncodeToString(first.SigningKey) != string(content) {
		t.Fatal("保存内容与生成密钥不一致")
	}
	before, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateJWTConfig(secretPath)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("重复加载更换了签名密钥或配置")
	}
	after, err := os.Stat(secretPath)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("复用已有密钥时改写了文件")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm()&0077 != 0 {
		t.Error("新建密钥向组或其他用户开放了权限")
	}
	entries, err := os.ReadDir(filepath.Dir(secretPath))
	if err != nil || len(entries) != 1 || entries[0].Name() != "jwt-secret" {
		t.Fatal("初始化后有临时文件残留")
	}
}

func TestLoadOrCreateJWTConfigEnvironmentOverride(t *testing.T) {
	for _, testCase := range []struct {
		name, secret string
		valid        bool
	}{
		{name: "valid", secret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x53}, 32)), valid: true},
		{name: "explicit empty"},
		{name: "invalid", secret: "private-invalid-environment-key"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clearJWTEnvironment(t)
			t.Setenv("CLOUDNEST_JWT_SECRET", testCase.secret)
			secretPath := filepath.Join(t.TempDir(), "absent", "jwt-secret")
			jwtConfig, err := LoadOrCreateJWTConfig(secretPath)
			if testCase.valid {
				if err != nil || !bytes.Equal(jwtConfig.SigningKey, bytes.Repeat([]byte{0x53}, 32)) {
					t.Fatal("环境密钥没有优先加载")
				}
			} else if err == nil || !reflect.DeepEqual(jwtConfig, JWTConfig{}) || strings.Contains(err.Error(), "private-invalid") {
				t.Fatal("无效环境密钥没有保持原有错误语义")
			}
			if _, err := os.Stat(filepath.Dir(secretPath)); !os.IsNotExist(err) {
				t.Fatal("环境覆盖路径产生了本地文件副作用")
			}
		})
	}
}

func TestLoadOrCreateJWTConfigExistingFile(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 48))
	for _, content := range []string{valid, "", "private-corrupt-file", valid + "\n"} {
		clearJWTEnvironment(t)
		secretPath := filepath.Join(t.TempDir(), "jwt-secret")
		if err := os.WriteFile(secretPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		jwtConfig, err := LoadOrCreateJWTConfig(secretPath)
		if content == valid {
			if err != nil || len(jwtConfig.SigningKey) != 48 {
				t.Error("已有合法密钥没有被复用")
			}
		} else if err == nil || !reflect.DeepEqual(jwtConfig, JWTConfig{}) || strings.Contains(err.Error(), "private-corrupt") {
			t.Error("坏文件没有返回零值配置和安全错误")
		}
		after, readError := os.ReadFile(secretPath)
		if readError != nil || string(after) != content {
			t.Error("已有文件内容被初始化操作覆盖")
		}
	}
}

func TestLoadOrCreateJWTConfigFailures(t *testing.T) {
	clearJWTEnvironment(t)
	parent := t.TempDir()
	parentFile := filepath.Join(parent, "parent-file")
	if err := os.WriteFile(parentFile, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, secretPath := range []string{"", " \t", parent, filepath.Join(parentFile, "jwt-secret"), filepath.Join(parent, "invalid\x00name")} {
		jwtConfig, err := LoadOrCreateJWTConfig(secretPath)
		if err == nil || !reflect.DeepEqual(jwtConfig, JWTConfig{}) {
			t.Error("无效路径被接受或返回部分配置")
		}
	}
	t.Setenv("ACCESS_TOKEN_TTL", "private-invalid-duration")
	secretPath := filepath.Join(parent, "not-created", "jwt-secret")
	jwtConfig, err := LoadOrCreateJWTConfig(secretPath)
	if err == nil || !reflect.DeepEqual(jwtConfig, JWTConfig{}) || strings.Contains(err.Error(), "private-invalid") {
		t.Error("配置错误没有在写文件前被拒绝")
	}
	if _, err := os.Stat(filepath.Dir(secretPath)); !os.IsNotExist(err) {
		t.Error("其余配置无效时创建了本地目录")
	}
}

func TestPublishJWTSecretKeepsWinnerAndCleansFailure(t *testing.T) {
	parent := t.TempDir()
	secretPath := filepath.Join(parent, "jwt-secret")
	if err := os.WriteFile(secretPath, []byte("existing-winner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishJWTSecret(secretPath, "losing-candidate"); err != nil {
		t.Fatal("正式文件已存在时未正确完成发布竞争处理")
	}
	content, err := os.ReadFile(secretPath)
	if err != nil || string(content) != "existing-winner" {
		t.Fatal("发布覆盖了已存在的文件")
	}
	// 超出本地文件名组件长度，验证 Link 失败后的临时文件清理。
	if err := publishJWTSecret(filepath.Join(parent, strings.Repeat("x", 300)), "fixture"); err == nil {
		t.Error("无效正式文件名发布成功")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "jwt-secret" {
		t.Error("竞争或发布失败后有临时文件残留")
	}
}

func TestLoadOrCreateJWTConfigConcurrent(t *testing.T) {
	clearJWTEnvironment(t)
	secretPath := filepath.Join(t.TempDir(), "CloudNest", "jwt-secret")
	type result struct {
		config JWTConfig
		err    error
	}
	results := make(chan result, 32)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range cap(results) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			jwtConfig, err := LoadOrCreateJWTConfig(secretPath)
			results <- result{config: jwtConfig, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var expected []byte
	for current := range results {
		if current.err != nil || len(current.config.SigningKey) != 32 {
			t.Fatalf("并发初始化失败: %v", current.err)
		}
		if expected == nil {
			expected = current.config.SigningKey
		}
		if !bytes.Equal(current.config.SigningKey, expected) {
			t.Error("并发初始化使用了不同密钥")
		}
	}
	content, err := os.ReadFile(secretPath)
	if err != nil || string(content) != base64.RawURLEncoding.EncodeToString(expected) {
		t.Fatal("并发返回值与最终文件不一致")
	}
	entries, err := os.ReadDir(filepath.Dir(secretPath))
	if err != nil || len(entries) != 1 {
		t.Error("并发初始化后有临时文件残留")
	}
}

func TestJWTConfigProcessChild(t *testing.T) {
	if os.Getenv("CLOUDNEST_JWT_TEST_CHILD") != "1" {
		t.Skip("由进程复用测试显式启动")
	}
	jwtConfig, err := LoadOrCreateJWTConfig(os.Getenv("CLOUDNEST_JWT_TEST_PATH"))
	if err != nil || len(jwtConfig.SigningKey) != 32 {
		t.Fatal("子进程密钥加载失败")
	}
	if expected := os.Getenv("CLOUDNEST_JWT_TEST_EXPECTED"); expected != "" && base64.RawURLEncoding.EncodeToString(jwtConfig.SigningKey) != expected {
		t.Fatal("新进程使用的密钥与已有文件不一致")
	}
}

func TestLoadOrCreateJWTConfigProcessReuse(t *testing.T) {
	clearJWTEnvironment(t)
	secretPath := filepath.Join(t.TempDir(), "CloudNest", "jwt-secret")
	for attempt := range 2 {
		command := exec.Command(os.Args[0], "-test.run=^TestJWTConfigProcessChild$")
		command.Env = append(os.Environ(), "CLOUDNEST_JWT_TEST_CHILD=1", "CLOUDNEST_JWT_TEST_PATH="+secretPath, "CLOUDNEST_JWT_TEST_EXPECTED=")
		if attempt == 1 {
			content, err := os.ReadFile(secretPath)
			if err != nil {
				t.Fatal("首次子进程没有保存密钥")
			}
			command.Env = append(command.Env, "CLOUDNEST_JWT_TEST_EXPECTED="+string(content))
		}
		if err := command.Run(); err != nil {
			t.Fatalf("新进程生成或复用验证失败: %v", err)
		}
	}
}
