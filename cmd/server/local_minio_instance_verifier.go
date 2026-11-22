package main

import (
	"context"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

const verifiedLocalMinIOImageID = "sha256:7c840b89c21e0af0e824cf57310becadbbd45c7fb407c78cd2ada4b74c76f733"

var localMinIOContainerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// localMinIOInstanceSnapshot 记录受信部署侧从同一容器读取的运行事实。
type localMinIOInstanceSnapshot struct {
	ContainerID    string
	ImageID        string
	ComposeProject string
	ComposeService string
	State          string
	StartedAt      time.Time
	Executable     string
	Arguments      []string
	S3Bindings     []netip.AddrPort // 容器 9000/tcp 的全部主机映射。
}

// newLocalMinIOSessionLockVerifier 为首版本机直连部署核对当次运行身份。
// readInstances 每次从受信的本机部署读取当前服务的完整快照，并确认镜像可执行文件沿用部署定义。
func newLocalMinIOSessionLockVerifier(
	composeProject string, readInstances func(context.Context) ([]localMinIOInstanceSnapshot, error),
) storage.MultipartSessionLockVerifier {
	return func(ctx context.Context, endpoint string) (string, error) {
		// 1. 仅接受带显式端口的规范 HTTP IPv4 回环端点。
		if ctx == nil {
			return "", domain.ErrUploadStorageUnavailable
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		hostAndPort, isHTTP := strings.CutPrefix(endpoint, "http://")
		address, err := netip.ParseAddrPort(hostAndPort)
		if composeProject == "" || readInstances == nil || !isHTTP || err != nil || address.Port() == 0 ||
			address.Addr() != netip.AddrFrom4([4]byte{127, 0, 0, 1}) || hostAndPort != address.String() {
			return "", nil
		}

		// 2. 每次读取新快照，先确定预期服务下唯一的运行实例。
		instances, err := readInstances(ctx)
		if stopError := ctx.Err(); stopError != nil {
			return "", stopError
		}
		if err != nil {
			return "", err
		}
		var current *localMinIOInstanceSnapshot
		for index := range instances {
			instance := &instances[index]
			if instance.ComposeProject != composeProject || instance.ComposeService != "minio" || instance.State != "running" {
				continue
			}
			if current != nil {
				return "", nil
			}
			current = instance
		}

		// 3. 核对固定镜像、进程与唯一映射，再用容器 ID 和启动时间组成身份。
		if current == nil || current.ImageID != verifiedLocalMinIOImageID ||
			!localMinIOContainerIDPattern.MatchString(current.ContainerID) || current.StartedAt.IsZero() ||
			current.Executable != "/usr/bin/minio" || !slices.Equal(current.Arguments, []string{"server", "/data", "--console-address", ":9001"}) ||
			len(current.S3Bindings) != 1 || current.S3Bindings[0] != address {
			return "", nil
		}
		return current.ContainerID + "/" + current.StartedAt.UTC().Format(time.RFC3339Nano), nil
	}
}
