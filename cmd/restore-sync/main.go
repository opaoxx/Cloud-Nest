// Command restore-sync 提供停服同步恢复使用的显式命令。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

const usage = "用法 restore-sync check -db-address <回环IP:端口> -database <库名> -server-uuid <实例UUID> -roster <清单路径> -mode <initial|resume>"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stdout, usage)
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "目标实例、库名和迁移记录校验通过")
}

func run(ctx context.Context, args []string) error {
	// 1. 解析显式参数，读取恢复专用凭据。
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return flag.ErrHelp
	}
	if len(args) == 0 || args[0] != "check" {
		return errors.New(usage)
	}
	options, err := config.ParseRestoreSyncResetOptions(args[1:])
	if err != nil {
		return err
	}
	username := os.Getenv("CLOUDNEST_RESTORE_DB_USER")
	password, passwordSet := os.LookupEnv("CLOUDNEST_RESTORE_DB_PASSWORD")
	if strings.TrimSpace(username) == "" || !passwordSet {
		return errors.New("必须显式设置 CLOUDNEST_RESTORE_DB_USER 和 CLOUDNEST_RESTORE_DB_PASSWORD")
	}

	// 2. 保留一条目标连接，核对实例 UUID 和库名。
	pool, err := repository.OpenMySQL(ctx, config.DatabaseConfig{
		Address: options.DatabaseAddress, DatabaseName: options.DatabaseName,
		Username: username, Password: password,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	connection, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取恢复目标专用连接失败: %w", err)
	}
	defer connection.Close()
	if _, err := repository.NewRestoredSyncStore(ctx, connection, options.ServerUUID, options.DatabaseName); err != nil {
		return err
	}

	// 3. 在同一连接检查迁移记录，返回结果后释放连接。
	return repository.CheckSchema(ctx, connection)
}
