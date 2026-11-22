package config

import (
	"os"
	"strings"
	"testing"
)

// clearStorageEnvironment 暂时移除配置变量，测试结束后由 testing 恢复原环境。
func clearStorageEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CLOUDNEST_STORAGE_PROVIDER", "CLOUDNEST_S3_ENDPOINT", "CLOUDNEST_S3_PUBLIC_ENDPOINT", "CLOUDNEST_S3_BUCKET", "CLOUDNEST_S3_ACCESS_KEY", "CLOUDNEST_S3_SECRET_KEY", "CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT"} {
		t.Setenv(name, "temporary-value")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadStorageConfigDefaults(t *testing.T) {
	// 1. 未设置环境变量时，完整采用本机已有容器和存储桶配置。
	clearStorageEnvironment(t)
	storageConfig, err := LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}

	// 2. 两个地址保留协议信息，凭证不作为测试失败内容输出。
	if storageConfig.Provider != "local_minio" || storageConfig.Bucket != "cloudnest" || storageConfig.EndpointURL.String() != "http://127.0.0.1:19000" || storageConfig.PublicEndpointURL.String() != "http://127.0.0.1:19000" || storageConfig.AccessKey != "root" || storageConfig.SecretKey != "rootroot" {
		t.Fatal("默认存储配置不符合预期")
	}
	if storageConfig.MinIOProofComposeProject != "" {
		t.Fatal("默认配置不应启用本机实例证明")
	}
}

func TestLoadStorageConfigMinIOProofComposeProject(t *testing.T) {
	clearStorageEnvironment(t)
	for _, project := range []string{"", "cloudnest", "cloudnest-proof_42"} {
		t.Setenv("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", project)
		settings, err := LoadStorageConfig()
		if err != nil || settings.MinIOProofComposeProject != project {
			t.Fatal("本机证明配置不符", err)
		}
	}
}

func TestLoadStorageConfigOverrides(t *testing.T) {
	// 1. 列出地址协议、公开地址继承和端口边界，验证规范化及凭证保真。
	clearStorageEnvironment(t)
	testCases := []struct {
		name          string
		endpoint      string
		publicAddress string
		wantEndpoint  string
		wantPublic    string
	}{
		{name: "bare host and inherited public address", endpoint: "localhost:19002", wantEndpoint: "http://localhost:19002", wantPublic: "http://localhost:19002"},
		{name: "HTTPS and default port", endpoint: "https://store.example.com/", wantEndpoint: "https://store.example.com", wantPublic: "https://store.example.com"},
		{name: "IPv6 and leading zero port", endpoint: "https://[::1]:019000/", wantEndpoint: "https://[::1]:19000", wantPublic: "https://[::1]:19000"},
		{name: "mixed protocols", endpoint: "127.0.0.1:19000", publicAddress: "https://files.example.com", wantEndpoint: "http://127.0.0.1:19000", wantPublic: "https://files.example.com"},
		{name: "bare public address", endpoint: "https://store.example.com", publicAddress: "localhost:19002", wantEndpoint: "https://store.example.com", wantPublic: "http://localhost:19002"},
		{name: "IPv6 default port", endpoint: "http://[::1]", wantEndpoint: "http://[::1]", wantPublic: "http://[::1]"},
		{name: "lowest port", endpoint: "http://localhost:1", wantEndpoint: "http://localhost:1", wantPublic: "http://localhost:1"},
		{name: "highest port", endpoint: "http://localhost:65535", wantEndpoint: "http://localhost:65535", wantPublic: "http://localhost:65535"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_S3_ENDPOINT", testCase.endpoint)
			if testCase.publicAddress != "" {
				t.Setenv("CLOUDNEST_S3_PUBLIC_ENDPOINT", testCase.publicAddress)
			}
			t.Setenv("CLOUDNEST_S3_BUCKET", "cloudnest.demo-1")
			t.Setenv("CLOUDNEST_S3_ACCESS_KEY", " custom-access ")
			t.Setenv("CLOUDNEST_S3_SECRET_KEY", "  secret@:/?# 密钥  ")
			storageConfig, err := LoadStorageConfig()
			if err != nil {
				t.Fatal(err)
			}

			// 2. 公开地址未设置时随内部地址变化，设置时可使用独立协议与主机。
			if storageConfig.EndpointURL.String() != testCase.wantEndpoint || storageConfig.PublicEndpointURL.String() != testCase.wantPublic || storageConfig.Bucket != "cloudnest.demo-1" || storageConfig.AccessKey != " custom-access " || storageConfig.SecretKey != "  secret@:/?# 密钥  " {
				t.Fatal("覆盖后的地址、桶名或原始凭证不符合预期")
			}
		})
	}
}

func TestLoadStorageConfigBucketLimits(t *testing.T) {
	// 1. 在允许长度的两端验证成功，防止边界值被误拒绝。
	clearStorageEnvironment(t)
	for _, bucketName := range []string{"abc", strings.Repeat("a", 63)} {
		t.Run(bucketName, func(t *testing.T) {
			t.Setenv("CLOUDNEST_S3_BUCKET", bucketName)
			storageConfig, err := LoadStorageConfig()
			if err != nil || storageConfig.Bucket != bucketName {
				t.Fatalf("合法桶名长度应被接受，error=%v", err)
			}
		})
	}
}

func TestLoadStorageConfigInvalid(t *testing.T) {
	// 1. 所有字段都覆盖显式空值，地址和桶名额外覆盖常见错误与边界。
	clearStorageEnvironment(t)
	testCases := []struct {
		name     string
		variable string
		value    string
	}{
		{name: "empty provider", variable: "CLOUDNEST_STORAGE_PROVIDER"},
		{name: "unsupported provider", variable: "CLOUDNEST_STORAGE_PROVIDER", value: "other-provider"},
		{name: "empty endpoint", variable: "CLOUDNEST_S3_ENDPOINT"},
		{name: "endpoint whitespace", variable: "CLOUDNEST_S3_ENDPOINT", value: "localhost :19000"},
		{name: "endpoint Unicode whitespace", variable: "CLOUDNEST_S3_ENDPOINT", value: "localhost:19000\u3000"},
		{name: "wrong protocol", variable: "CLOUDNEST_S3_ENDPOINT", value: "ftp://localhost:19000"},
		{name: "missing host", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://:19000"},
		{name: "path", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://localhost:19000/cloudnest"},
		{name: "query", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://localhost:19000?token=do-not-log"},
		{name: "empty query marker", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://localhost:19000?"},
		{name: "fragment", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://localhost:19000#do-not-log"},
		{name: "credentials", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://access:do-not-log@localhost:19000"},
		{name: "parse error containing credentials", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://access:do-not-log@localhost:bad"},
		{name: "empty port", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://localhost:"},
		{name: "zero port", variable: "CLOUDNEST_S3_ENDPOINT", value: "localhost:0"},
		{name: "port overflow", variable: "CLOUDNEST_S3_ENDPOINT", value: "localhost:65536"},
		{name: "unbracketed IPv6", variable: "CLOUDNEST_S3_ENDPOINT", value: "::1"},
		{name: "invalid bracketed host", variable: "CLOUDNEST_S3_ENDPOINT", value: "http://[localhost]:19000"},
		{name: "empty public endpoint", variable: "CLOUDNEST_S3_PUBLIC_ENDPOINT"},
		{name: "invalid public endpoint", variable: "CLOUDNEST_S3_PUBLIC_ENDPOINT", value: "https://files.example.com?key=do-not-log"},
		{name: "empty bucket", variable: "CLOUDNEST_S3_BUCKET"},
		{name: "short bucket", variable: "CLOUDNEST_S3_BUCKET", value: "ab"},
		{name: "long bucket", variable: "CLOUDNEST_S3_BUCKET", value: strings.Repeat("a", 64)},
		{name: "uppercase bucket", variable: "CLOUDNEST_S3_BUCKET", value: "CloudNest"},
		{name: "underscore bucket", variable: "CLOUDNEST_S3_BUCKET", value: "cloud_nest"},
		{name: "bucket starts with dash", variable: "CLOUDNEST_S3_BUCKET", value: "-cloudnest"},
		{name: "bucket ends with dash", variable: "CLOUDNEST_S3_BUCKET", value: "cloudnest-"},
		{name: "IPv4 bucket", variable: "CLOUDNEST_S3_BUCKET", value: "127.0.0.1"},
		{name: "IPv4 shaped bucket", variable: "CLOUDNEST_S3_BUCKET", value: "999.999.999.999"},
		{name: "adjacent dots", variable: "CLOUDNEST_S3_BUCKET", value: "cloud..nest"},
		{name: "dot dash", variable: "CLOUDNEST_S3_BUCKET", value: "cloud.-nest"},
		{name: "dash dot", variable: "CLOUDNEST_S3_BUCKET", value: "cloud-.nest"},
		{name: "empty access key", variable: "CLOUDNEST_S3_ACCESS_KEY"},
		{name: "blank access key", variable: "CLOUDNEST_S3_ACCESS_KEY", value: " \t"},
		{name: "empty secret key", variable: "CLOUDNEST_S3_SECRET_KEY"},
		{name: "blank secret key", variable: "CLOUDNEST_S3_SECRET_KEY", value: " \t"},
		{name: "invalid proof project", variable: "CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", value: "do-not-log\nproject"},
		{name: "uppercase proof project", variable: "CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", value: "CloudNest"},
		{name: "proof project starts with dash", variable: "CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", value: "-cloudnest"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_S3_ACCESS_KEY", "do-not-log-access")
			t.Setenv("CLOUDNEST_S3_SECRET_KEY", "do-not-log-secret")
			t.Setenv(testCase.variable, testCase.value)
			storageConfig, err := LoadStorageConfig()

			// 2. 返回零值及配置名，不回显原始地址中的凭证或解析错误中的输入。
			if err == nil {
				t.Fatal("预期无效存储配置被拒绝")
			}
			if storageConfig != (StorageConfig{}) || !strings.Contains(err.Error(), testCase.variable) {
				t.Fatal("错误结果未说明配置名，或返回了部分配置")
			}
			if strings.Contains(err.Error(), "do-not-log") {
				t.Fatal("错误信息包含不应回显的输入")
			}
		})
	}
}
