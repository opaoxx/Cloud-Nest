package config

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/google/uuid"
)

// RestoreSyncResetMode 区分首次记录清单和使用原清单重跑。
type RestoreSyncResetMode string

const (
	RestoreSyncResetInitial RestoreSyncResetMode = "initial"
	RestoreSyncResetResume  RestoreSyncResetMode = "resume"
)

// RestoreSyncResetOptions 保存离线恢复命令明确指定的目标与运行模式。
type RestoreSyncResetOptions struct {
	DatabaseAddress string
	DatabaseName    string
	ServerUUID      uuid.UUID
	RosterPath      string
	Mode            RestoreSyncResetMode
}

// ParseRestoreSyncResetOptions 校验离线恢复命令参数的格式。
// 所有选项都由命令行提供。实际目标和停服状态仍需在换代前单独核对。
func ParseRestoreSyncResetOptions(args []string) (RestoreSyncResetOptions, error) {
	// 1. 读取显式参数，拒绝重复、未知参数和多余输入。
	flags := flag.NewFlagSet("restore-sync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options RestoreSyncResetOptions
	var mode, serverUUID string
	seen := make(map[string]bool)
	register := func(name, description string, destination *string) {
		flags.Func(name, description, func(value string) error {
			if seen[name] {
				return fmt.Errorf("参数 -%s 只能提供一次", name)
			}
			seen[name] = true
			*destination = value
			return nil
		})
	}
	register("db-address", "目标库的回环 IP 和端口", &options.DatabaseAddress)
	register("database", "已核对的目标库名", &options.DatabaseName)
	register("server-uuid", "隔离恢复记录中的 MySQL 实例 UUID", &serverUUID)
	register("roster", "首次记录的设备清单路径", &options.RosterPath)
	register("mode", "initial 首次保存，resume 使用原清单重跑", &mode)
	if err := flags.Parse(args); err != nil {
		return RestoreSyncResetOptions{}, err
	}
	if flags.NArg() != 0 {
		return RestoreSyncResetOptions{}, fmt.Errorf("离线恢复参数不接受位置参数")
	}

	// 2. 校验回环地址、库名、清单路径和运行模式。
	address, err := netip.ParseAddrPort(options.DatabaseAddress)
	if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 {
		return RestoreSyncResetOptions{}, fmt.Errorf("必须通过 -db-address 指定回环 IP 和 1～65535 的端口")
	}
	if options.DatabaseName == "" || options.DatabaseName != strings.TrimSpace(options.DatabaseName) {
		return RestoreSyncResetOptions{}, fmt.Errorf("必须通过 -database 指定目标库名，且首尾不能有空白")
	}
	options.ServerUUID, err = uuid.Parse(serverUUID)
	if err != nil || options.ServerUUID == uuid.Nil {
		return RestoreSyncResetOptions{}, fmt.Errorf("必须通过 -server-uuid 指定非零 MySQL 实例 UUID")
	}
	if strings.TrimSpace(options.RosterPath) == "" {
		return RestoreSyncResetOptions{}, fmt.Errorf("必须通过 -roster 指定原始清单路径")
	}
	options.Mode = RestoreSyncResetMode(mode)
	if options.Mode != RestoreSyncResetInitial && options.Mode != RestoreSyncResetResume {
		return RestoreSyncResetOptions{}, fmt.Errorf("必须通过 -mode 选择 initial 或 resume")
	}

	// 3. 全部通过后，返回恢复选项。
	options.DatabaseAddress = address.String()
	return options, nil
}
