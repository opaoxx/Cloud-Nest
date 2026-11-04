package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// DatabaseConfig 保存服务端连接 MySQL 所需的配置。
type DatabaseConfig struct {
	// Address 是包含明确主机和端口的 TCP 地址。
	Address string
	// DatabaseName 是需要连接的数据库名称。
	DatabaseName string
	// Username 是 MySQL 连接账号，与云盘业务账号分开管理。
	Username string
	// Password 是原始连接密码，只用于建立连接，不写入日志。
	Password string
}

// LoadDatabaseConfig 读取 CLOUDNEST_DB_* 环境变量并校验连接配置。
// 变量未设置时采用本地开发默认值，校验失败返回零值配置和错误。
// 本方法只检查配置格式，不建立连接，也不验证数据库或账号是否存在。
func LoadDatabaseConfig() (DatabaseConfig, error) {
	// 1. 读取环境变量，未设置时使用本地开发默认值。
	databaseConfig := DatabaseConfig{
		Address:      envOrDefault("CLOUDNEST_DB_ADDRESS", "127.0.0.1:13309"),
		DatabaseName: envOrDefault("CLOUDNEST_DB_NAME", "cloudnest"),
		Username:     envOrDefault("CLOUDNEST_DB_USER", "root"),
		Password:     envOrDefault("CLOUDNEST_DB_PASSWORD", "root"),
	}

	// 2. 校验数据库地址中的主机和数字端口。
	if strings.ContainsAny(databaseConfig.Address, " \t\r\n") {
		return DatabaseConfig{}, fmt.Errorf("CLOUDNEST_DB_ADDRESS 不能包含空白")
	}
	host, portText, err := net.SplitHostPort(databaseConfig.Address)
	if err != nil {
		return DatabaseConfig{}, fmt.Errorf("CLOUDNEST_DB_ADDRESS 应为 host:port，IPv6 地址需要方括号")
	}
	if host == "" {
		return DatabaseConfig{}, fmt.Errorf("CLOUDNEST_DB_ADDRESS 必须指定主机")
	}
	portNumber, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || portNumber == 0 {
		return DatabaseConfig{}, fmt.Errorf("CLOUDNEST_DB_ADDRESS 的端口必须是 1～65535 的数字")
	}

	// 3. 检查库名与用户名，保留密码原值。
	if databaseConfig.DatabaseName == "" || databaseConfig.DatabaseName != strings.TrimSpace(databaseConfig.DatabaseName) {
		return DatabaseConfig{}, fmt.Errorf("CLOUDNEST_DB_NAME 不能为空，也不能包含首尾空白")
	}
	if strings.TrimSpace(databaseConfig.Username) == "" {
		return DatabaseConfig{}, fmt.Errorf("CLOUDNEST_DB_USER 不能为空或全空白")
	}

	// 4. 返回规范化后的配置，交给启动入口使用。
	databaseConfig.Address = net.JoinHostPort(host, strconv.FormatUint(portNumber, 10))
	return databaseConfig, nil
}

// envOrDefault 仅在变量未设置时使用默认值，保留显式设置的空字符串。
func envOrDefault(name, defaultValue string) string {
	value, exists := os.LookupEnv(name)
	if exists {
		return value
	}
	return defaultValue
}
