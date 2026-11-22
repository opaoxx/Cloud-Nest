package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// bucketNamePattern 限定桶名长度、字符范围与首尾字符。
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// bucketIPPattern 拒绝 IPv4 样式名称，包括不能解析为合法 IP 的四段数字。
var bucketIPPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)

var minIOProofComposeProjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// StorageConfig 保存本项目 MinIO 对象存储的访问配置。
type StorageConfig struct {
	// Provider 是数据库对象记录使用的存储逻辑标识，当前仅支持 local_minio。
	Provider string
	// EndpointURL 是后端访问存储的根地址，包含 HTTP 或 HTTPS 协议。
	EndpointURL *url.URL
	// PublicEndpointURL 是客户端访问存储的根地址，用于后续生成下载链接。
	PublicEndpointURL *url.URL
	// Bucket 是承载项目文件对象的存储桶名称。
	Bucket string
	// AccessKey 是服务端连接凭证的一部分，不写入日志。
	AccessKey string
	// SecretKey 是服务端连接密钥，原样保留且不写入日志。
	SecretKey string
	// MinIOProofComposeProject 显式选择本机 MinIO 运行证明的 Compose 项目，空值保持待确认路径。
	MinIOProofComposeProject string
}

// LoadStorageConfig 读取 CLOUDNEST_STORAGE_PROVIDER、CLOUDNEST_S3_* 和可选的本机实例证明配置。
// 未设置的变量使用开发默认值；失败返回零值配置与不含原始输入的错误。
// 本方法只验证格式，不创建客户端、不访问 MinIO，也不检查桶或凭证是否可用。
func LoadStorageConfig() (StorageConfig, error) {
	// 1. 读取环境变量，未设置时使用本地开发默认值。
	endpointText := envOrDefault("CLOUDNEST_S3_ENDPOINT", "127.0.0.1:19000")
	publicEndpointText := envOrDefault("CLOUDNEST_S3_PUBLIC_ENDPOINT", endpointText)
	storageConfig := StorageConfig{
		Provider:  envOrDefault("CLOUDNEST_STORAGE_PROVIDER", "local_minio"),
		Bucket:    envOrDefault("CLOUDNEST_S3_BUCKET", "cloudnest"),
		AccessKey: envOrDefault("CLOUDNEST_S3_ACCESS_KEY", "root"),
		SecretKey: envOrDefault("CLOUDNEST_S3_SECRET_KEY", "rootroot"),
	}

	// 2. 校验 provider，并解析后端和客户端访问地址。
	if storageConfig.Provider != "local_minio" {
		return StorageConfig{}, fmt.Errorf("CLOUDNEST_STORAGE_PROVIDER 当前只支持 local_minio")
	}
	endpointURL, err := parseStorageEndpoint("CLOUDNEST_S3_ENDPOINT", endpointText)
	if err != nil {
		return StorageConfig{}, err
	}
	publicEndpointURL, err := parseStorageEndpoint("CLOUDNEST_S3_PUBLIC_ENDPOINT", publicEndpointText)
	if err != nil {
		return StorageConfig{}, err
	}

	// 3. 校验桶名及凭证，保留凭证原值。
	if !bucketNamePattern.MatchString(storageConfig.Bucket) {
		return StorageConfig{}, fmt.Errorf("CLOUDNEST_S3_BUCKET 必须为 3～63 位小写字母、数字、点或连字符，且首尾为字母或数字")
	}
	if bucketIPPattern.MatchString(storageConfig.Bucket) || strings.Contains(storageConfig.Bucket, "..") || strings.Contains(storageConfig.Bucket, ".-") || strings.Contains(storageConfig.Bucket, "-.") {
		return StorageConfig{}, fmt.Errorf("CLOUDNEST_S3_BUCKET 不能呈 IPv4 样式，也不能包含 ..、.- 或 -.")
	}
	if strings.TrimSpace(storageConfig.AccessKey) == "" {
		return StorageConfig{}, fmt.Errorf("CLOUDNEST_S3_ACCESS_KEY 不能为空或全空白")
	}
	if strings.TrimSpace(storageConfig.SecretKey) == "" {
		return StorageConfig{}, fmt.Errorf("CLOUDNEST_S3_SECRET_KEY 不能为空或全空白")
	}

	// 4. 校验显式选择的 Compose 项目，空值表示不启用证明来源。
	storageConfig.MinIOProofComposeProject = os.Getenv("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT")
	if storageConfig.MinIOProofComposeProject != "" && !minIOProofComposeProjectPattern.MatchString(storageConfig.MinIOProofComposeProject) {
		return StorageConfig{}, fmt.Errorf("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT 必须是有效的 Compose 项目名")
	}

	// 5. 返回存储配置，交给启动入口使用。
	storageConfig.EndpointURL = endpointURL
	storageConfig.PublicEndpointURL = publicEndpointURL
	return storageConfig, nil
}

// parseStorageEndpoint 解析无凭证、路径参数或片段的 HTTP(S) 根地址。
// 不回显解析库的原始错误，避免错误地址中误填的密钥进入日志。
func parseStorageEndpoint(variableName, value string) (*url.URL, error) {
	// 1. 拒绝空值和空白，为省略协议的本地地址补上 HTTP。
	if value == "" || strings.ContainsFunc(value, unicode.IsSpace) {
		return nil, fmt.Errorf("%s 不能为空或包含空白", variableName)
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}

	// 2. 只接受有明确主机的 HTTP(S) 根地址，不允许 URL 携带凭证或参数。
	endpointURL, err := url.Parse(value)
	if err != nil || (endpointURL.Scheme != "http" && endpointURL.Scheme != "https") || endpointURL.Hostname() == "" {
		return nil, fmt.Errorf("%s 必须是包含主机的 HTTP 或 HTTPS 服务地址", variableName)
	}
	if endpointURL.User != nil || (endpointURL.Path != "" && endpointURL.Path != "/") || endpointURL.RawPath != "" || strings.ContainsAny(value, "?#") {
		return nil, fmt.Errorf("%s 必须是根地址，不能包含账号密码、业务路径、查询参数或片段", variableName)
	}
	hostname := endpointURL.Hostname()
	if strings.Contains(hostname, ":") || strings.HasPrefix(endpointURL.Host, "[") {
		if net.ParseIP(hostname) == nil || !strings.HasPrefix(endpointURL.Host, "[") {
			return nil, fmt.Errorf("%s 的 IPv6 主机必须是有效地址并使用方括号", variableName)
		}
	}

	// 3. 显式端口必须有效；未填写时采用对应协议的默认端口语义。
	portText := endpointURL.Port()
	if strings.HasSuffix(endpointURL.Host, ":") {
		return nil, fmt.Errorf("%s 的显式端口不能为空", variableName)
	}
	if portText != "" {
		portNumber, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || portNumber == 0 {
			return nil, fmt.Errorf("%s 的端口必须是 1～65535 的数字", variableName)
		}
		endpointURL.Host = net.JoinHostPort(hostname, strconv.FormatUint(portNumber, 10))
	}
	endpointURL.Path = ""
	return endpointURL, nil
}
