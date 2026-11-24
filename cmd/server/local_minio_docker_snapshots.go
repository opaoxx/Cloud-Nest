package main

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const localDockerDesktopHost = "npipe:////./pipe/dockerDesktopLinuxEngine"

// Docker 模板只投影实例核对需要的字段，包括可能覆盖镜像内文件的挂载位置。
const localMinIODockerInspectFields = `{"id":{{json .Id}},"image":{{json .Image}},` +
	`"project":{{json (index .Config.Labels "com.docker.compose.project")}},"service":{{json (index .Config.Labels "com.docker.compose.service")}},` +
	`"state":{{json .State.Status}},"started_at":{{json .State.StartedAt}},"executable":{{json .Path}},"args":{{json .Args}},` +
	`"bindings":{{json (index .NetworkSettings.Ports "9000/tcp")}},"mounts":[{{$separator := ""}}` +
	`{{range .Mounts}}{{$separator}}{{json .Destination}}{{$separator = ","}}{{end}}` +
	`{{range $destination, $_ := index .HostConfig "Tmpfs"}}{{$separator}}{{json $destination}}{{$separator = ","}}{{end}}]}`

type localMinIODockerInstance struct {
	ContainerID string    `json:"id"`
	ImageID     string    `json:"image"`
	Project     string    `json:"project"`
	Service     string    `json:"service"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	Executable  string    `json:"executable"`
	Arguments   []string  `json:"args"`
	Bindings    []struct {
		IP   string `json:"HostIp"`
		Port string `json:"HostPort"`
	} `json:"bindings"`
	Mounts []string `json:"mounts"`
}

// newLocalMinIODockerSnapshotReader 从本机 Docker Desktop Linux 引擎读取选定 Compose 项目的 MinIO 实例。
func newLocalMinIODockerSnapshotReader(composeProject string) func(context.Context) ([]localMinIOInstanceSnapshot, error) {
	return func(ctx context.Context) ([]localMinIOInstanceSnapshot, error) {
		return readLocalMinIODockerSnapshots(ctx, composeProject, runLocalDockerCommand)
	}
}

func runLocalDockerCommand(ctx context.Context, arguments ...string) ([]byte, error) {
	// 明确连接既有本机引擎，清除子进程的 Docker 连接覆盖。
	command := exec.CommandContext(ctx, "docker", append([]string{"--host", localDockerDesktopHost}, arguments...)...)
	for _, variable := range command.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !strings.EqualFold(name, "DOCKER_HOST") && !strings.EqualFold(name, "DOCKER_CONTEXT") {
			command.Env = append(command.Env, variable)
		}
	}
	return command.Output()
}

func readLocalMinIODockerSnapshots(
	ctx context.Context, composeProject string, runDocker func(context.Context, ...string) ([]byte, error),
) ([]localMinIOInstanceSnapshot, error) {
	// 1. 使用两个 Compose 标签查找容器，保留完整 ID 供随后核对。
	if ctx == nil || composeProject == "" {
		return nil, domain.ErrUploadStorageUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, err := runDocker(ctx, "container", "ls", "--all", "--no-trunc", "--format", "{{.ID}}",
		"--filter", "label=com.docker.compose.project="+composeProject, "--filter", "label=com.docker.compose.service=minio")
	if err != nil {
		return nil, localMinIODockerReadError(ctx)
	}
	containerIDs := strings.Fields(string(output))
	if len(containerIDs) == 0 {
		return nil, ctx.Err()
	}
	for _, containerID := range containerIDs {
		if !localMinIOContainerIDPattern.MatchString(containerID) {
			return nil, domain.ErrUploadStorageUnavailable
		}
	}

	// 2. 只读取证明所需字段，确认返回记录与这次列出的实例一一对应。
	output, err = runDocker(ctx, append([]string{"container", "inspect", "--format", localMinIODockerInspectFields}, containerIDs...)...)
	if err != nil {
		return nil, localMinIODockerReadError(ctx)
	}
	rows := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(rows) != len(containerIDs) {
		return nil, domain.ErrUploadStorageUnavailable
	}
	snapshots := make([]localMinIOInstanceSnapshot, 0, len(rows))
	for index, row := range rows {
		var instance localMinIODockerInstance
		if err := json.Unmarshal([]byte(row), &instance); err != nil || instance.ContainerID != containerIDs[index] ||
			instance.Project != composeProject || instance.Service != "minio" {
			return nil, domain.ErrUploadStorageUnavailable
		}
		snapshot, err := instance.snapshot(ctx, runDocker)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func (instance localMinIODockerInstance) snapshot(
	ctx context.Context, runDocker func(context.Context, ...string) ([]byte, error),
) (localMinIOInstanceSnapshot, error) {
	// 1. 保存实际端口映射和身份字段，保留核对器判断服务是否唯一所需的运行状态。
	snapshot := localMinIOInstanceSnapshot{
		ContainerID: instance.ContainerID, ImageID: instance.ImageID, ComposeProject: instance.Project, ComposeService: instance.Service,
		State: instance.State, StartedAt: instance.StartedAt, Arguments: instance.Arguments,
	}
	for _, binding := range instance.Bindings {
		address, err := netip.ParseAddrPort(net.JoinHostPort(binding.IP, binding.Port))
		if err != nil {
			return localMinIOInstanceSnapshot{}, domain.ErrUploadStorageUnavailable
		}
		snapshot.S3Bindings = append(snapshot.S3Bindings, address)
	}
	if instance.State != "running" || instance.ImageID != verifiedLocalMinIOImageID || instance.Executable != "/usr/bin/minio" {
		return snapshot, nil
	}

	// 2. 检查挂载与可写层，只有镜像内的可执行文件未被覆盖时才交付该路径。
	for _, destination := range instance.Mounts {
		if minIOExecutablePathCovered(instance.Executable, destination) {
			return snapshot, nil
		}
	}
	changes, err := runDocker(ctx, "container", "diff", instance.ContainerID)
	if err != nil {
		return localMinIOInstanceSnapshot{}, localMinIODockerReadError(ctx)
	}
	for _, change := range strings.Split(strings.TrimRight(string(changes), "\r\n"), "\n") {
		if change == "" {
			continue
		}
		kind, destination, found := strings.Cut(strings.TrimSuffix(change, "\r"), " ")
		if !found || (kind != "A" && kind != "C" && kind != "D") || minIOExecutablePathCovered(instance.Executable, destination) {
			return snapshot, nil
		}
	}
	snapshot.Executable = instance.Executable
	return snapshot, nil
}

func minIOExecutablePathCovered(executable, destination string) bool {
	cleaned := path.Clean(destination)
	return !path.IsAbs(cleaned) || cleaned == "/" || executable == cleaned || strings.HasPrefix(executable, cleaned+"/")
}

func localMinIODockerReadError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return domain.ErrUploadStorageUnavailable
}
