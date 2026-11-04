package repository

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
)

func TestOpenMySQLCanceledContext(t *testing.T) {
	// 1. 提前取消上下文，不依赖本机是否运行 MySQL。
	checkContext, cancel := context.WithCancel(context.Background())
	cancel()
	databaseConfig := config.DatabaseConfig{Address: "127.0.0.1:13309", DatabaseName: "cloudnest", Username: "root", Password: "root"}

	// 2. 已取消的检查必须失败，不返回可被误用的连接池。
	connectionPool, err := OpenMySQL(checkContext, databaseConfig)
	if connectionPool != nil {
		_ = connectionPool.Close()
		t.Fatal("检查取消后仍返回了连接池")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("预期取消错误，实际为 %v", err)
	}
}

func TestOpenMySQLHandshakeTimeout(t *testing.T) {
	// 1. 使用只接受 TCP、却不回复 MySQL 握手的本地端口，验证超时上限。
	testCases := []struct {
		name          string
		callerTimeout time.Duration
		minimumWait   time.Duration
		maximumWait   time.Duration
	}{
		{name: "caller shorter deadline", callerTimeout: 100 * time.Millisecond, maximumWait: 2 * time.Second},
		{name: "default five second limit", minimumWait: 4 * time.Second, maximumWait: 8 * time.Second},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			checkContext := context.Background()
			if testCase.callerTimeout > 0 {
				var cancel context.CancelFunc
				checkContext, cancel = context.WithTimeout(checkContext, testCase.callerTimeout)
				defer cancel()
			}
			databaseConfig := config.DatabaseConfig{Address: listener.Addr().String(), DatabaseName: "cloudnest", Username: "root", Password: "root"}

			// 2. 检查失败结果与等待时间，不能把仅能建立 TCP 连接视为 MySQL 可用。
			startedAt := time.Now()
			connectionPool, err := OpenMySQL(checkContext, databaseConfig)
			elapsed := time.Since(startedAt)
			if connectionPool != nil {
				_ = connectionPool.Close()
				t.Fatal("握手未完成却返回了连接池")
			}
			if err == nil || elapsed < testCase.minimumWait || elapsed > testCase.maximumWait {
				t.Fatalf("超时检查不符合预期，elapsed=%s，error=%v", elapsed, err)
			}
			if testCase.callerTimeout > 0 && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("未保留调用方超时语义，error=%v", err)
			}

			// 3. 从服务端确认超时后的客户端连接已关闭，避免失败时遗留连接。
			if err := listener.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			connection, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			readBuffer := make([]byte, 1)
			if _, err := connection.Read(readBuffer); !errors.Is(err, io.EOF) {
				t.Fatalf("预期超时连接已关闭，error=%v", err)
			}
		})
	}
}

func TestOpenMySQLIntegration(t *testing.T) {
	// 1. 仅在显式启用时读取现有数据库配置，不自动创建库或修改业务表。
	if os.Getenv("CLOUDNEST_MYSQL_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MYSQL_TEST=1 后执行真实 MySQL 只读验证")
	}
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	checkContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connectionPool, err := OpenMySQL(checkContext, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connectionPool.Close() })

	// 2. 通过只读查询验证目标库、会话时区、字符集及日期解析。
	var databaseName, sessionTimeZone, characterSet string
	var parsedTime time.Time
	err = connectionPool.QueryRowContext(checkContext, "SELECT DATABASE(), @@session.time_zone, @@character_set_connection, CAST('2026-11-04 12:00:00' AS DATETIME)").Scan(&databaseName, &sessionTimeZone, &characterSet, &parsedTime)
	if err != nil {
		t.Fatal(err)
	}
	if databaseName != databaseConfig.DatabaseName || sessionTimeZone != "+00:00" || characterSet != "utf8mb4" || parsedTime.Location() != time.UTC || parsedTime.Hour() != 12 {
		t.Fatal("目标库、会话时区、字符集或时间解析不符合预期")
	}
	if statistics := connectionPool.Stats(); statistics.MaxOpenConnections != 10 || statistics.OpenConnections != 1 || statistics.Idle != 1 {
		t.Fatalf("初始池状态不符合预期：%+v", statistics)
	}

	// 3. 显式关闭池，确认连接归零并拒绝后续使用。
	if err := connectionPool.Close(); err != nil {
		t.Fatal(err)
	}
	if connectionPool.Stats().OpenConnections != 0 || connectionPool.PingContext(checkContext) == nil {
		t.Fatal("关闭后的连接池仍可使用或未释放连接")
	}
}
