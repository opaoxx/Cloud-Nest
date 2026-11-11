package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func setServerConfigDirectory(t *testing.T, directory string) string {
	t.Helper()
	variable := "XDG_CONFIG_HOME"
	switch runtime.GOOS {
	case "windows":
		variable = "APPDATA"
	case "darwin", "ios":
		variable = "HOME"
	case "plan9":
		variable = "home"
	}
	t.Setenv(variable, directory)
	resolved, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestLoadServerJWTConfigPersistence(t *testing.T) {
	clearJWTEnvironment(t)
	directory := t.TempDir()
	directory = setServerConfigDirectory(t, directory)
	first, err := LoadServerJWTConfig()
	if err != nil || len(first.SigningKey) != 32 {
		t.Fatal("启动配置未初始化本地密钥")
	}
	path := filepath.Join(directory, "CloudNest", "jwt-secret")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadServerJWTConfig()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("重复启动未复用同一配置和密钥")
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("再次加载改写了密钥文件")
	}
}

func TestLoadServerJWTConfigEnvironment(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "explicit empty"}[valid], func(t *testing.T) {
			clearJWTEnvironment(t)
			blocked := filepath.Join(t.TempDir(), "configuration-file")
			if err := os.WriteFile(blocked, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			setServerConfigDirectory(t, blocked)
			secret := ""
			if valid {
				secret = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
			}
			t.Setenv("CLOUDNEST_JWT_SECRET", secret)
			loaded, err := LoadServerJWTConfig()
			if valid {
				if err != nil || !bytes.Equal(loaded.SigningKey, bytes.Repeat([]byte{7}, 32)) {
					t.Fatal("环境密钥被无效本地目录阻断")
				}
			} else if err == nil || !reflect.DeepEqual(loaded, JWTConfig{}) {
				t.Fatal("显式空环境密钥未保留校验语义")
			}
		})
	}
}

func TestLoadServerJWTConfigPathFailure(t *testing.T) {
	clearJWTEnvironment(t)
	blocked := filepath.Join(t.TempDir(), "configuration-file")
	if err := os.WriteFile(blocked, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	setServerConfigDirectory(t, blocked)
	loaded, err := LoadServerJWTConfig()
	if err == nil || !reflect.DeepEqual(loaded, JWTConfig{}) {
		t.Fatal("本地配置目录故障未返回错误")
	}
}
