// Command restore-sync 提供停服同步恢复使用的显式命令。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const usage = "用法 restore-sync <check|reset> -db-address <回环IP:端口> -database <库名> -server-uuid <实例UUID> -roster <清单路径> -mode <initial|resume>"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stdout, usage)
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	// 1. 解析显式参数，读取恢复专用凭据。
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return flag.ErrHelp
	}
	if len(args) == 0 || (args[0] != "check" && args[0] != "reset") {
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
	store, err := repository.NewRestoredSyncStore(ctx, connection, options.ServerUUID, options.DatabaseName)
	if err != nil {
		return err
	}

	// 3. 在同一连接检查迁移记录，通过后才按子命令执行。
	if err := repository.CheckSchema(ctx, connection); err != nil {
		return err
	}
	if args[0] == "check" {
		fmt.Fprintln(output, "目标实例、库名和迁移记录校验通过")
		return nil
	}
	return resetSyncDevices(ctx, options, store, output)
}

func resetSyncDevices(ctx context.Context, options config.RestoreSyncResetOptions, store *repository.Store, output io.Writer) error {
	// 1. 首次运行安排写入前保存清单，重跑先读取原清单。
	var imported []domain.RestoredSyncDeviceCandidate
	var saveImported func(context.Context, []domain.RestoredSyncDeviceCandidate) error
	if options.Mode == config.RestoreSyncResetResume {
		var err error
		imported, err = service.LoadRestoredSyncDeviceRoster(options.RosterPath)
		if err != nil {
			return err
		}
	} else {
		saveImported = func(saveContext context.Context, roster []domain.RestoredSyncDeviceCandidate) error {
			if err := saveContext.Err(); err != nil {
				return err
			}
			return service.SaveRestoredSyncDeviceRoster(options.RosterPath, roster)
		}
	}

	// 2. 复用停服换代流程，逐批提交后完整读回。
	const batchSize = domain.MaxRestoredSyncDeviceCandidates
	const pageSize = domain.MaxRestoredSyncDeviceCandidates
	result, err := service.RunRestoredSyncDeviceReset(ctx, imported, batchSize, pageSize,
		store.ListRestoredSyncDeviceCandidates, store.ResetRestoredSyncDevice, saveImported)

	// 3. 报告实际进度，只有全量复核成功才报告完成。
	if err != nil {
		return fmt.Errorf("同步换代未完成（已提交 %d 条，复核通过 %d 条）: %w", result.Reset, result.Verified, err)
	}
	fmt.Fprintf(output, "同步换代完成，已提交 %d 条，复核通过 %d 条\n", result.Reset, result.Verified)
	return nil
}
