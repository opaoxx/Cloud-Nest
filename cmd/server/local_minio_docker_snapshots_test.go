package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestReadLocalMinIODockerSnapshots(t *testing.T) {
	containerID := strings.Repeat("ab", 32)
	for _, scenario := range []struct {
		name, mounts, changes string
		wantProof, wantDiff   bool
	}{
		{name: "original executable", mounts: `["/data"]`, changes: "A /tmp\r\n", wantProof: true, wantDiff: true},
		{name: "mounted executable", mounts: `["/usr/bin/minio"]`},
		{name: "mounted executable directory", mounts: `["/usr/bin"]`},
		{name: "changed executable", mounts: `[]`, changes: "C /usr/bin/minio\n", wantDiff: true},
		{name: "changed executable ancestor", mounts: `[]`, changes: "C /usr\n", wantDiff: true},
		{name: "invalid diff", mounts: `[]`, changes: "unknown\n", wantDiff: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			diffCalls := 0
			read := func(ctx context.Context) ([]localMinIOInstanceSnapshot, error) {
				return readLocalMinIODockerSnapshots(ctx, "fixture", func(_ context.Context, arguments ...string) ([]byte, error) {
					switch arguments[1] {
					case "ls":
						want := []string{"container", "ls", "--all", "--no-trunc", "--format", "{{.ID}}",
							"--filter", "label=com.docker.compose.project=fixture", "--filter", "label=com.docker.compose.service=minio"}
						if !slices.Equal(arguments, want) {
							t.Fatalf("容器查询未限定项目和服务，得到 %q", arguments)
						}
						return []byte(containerID + "\r\n"), nil
					case "inspect":
						if !slices.Equal(arguments, []string{"container", "inspect", "--format", localMinIODockerInspectFields, containerID}) {
							t.Fatalf("inspect 未按完整 ID 投影，得到 %q", arguments)
						}
						row := fmt.Sprintf(`{"id":%q,"image":%q,"project":"fixture","service":"minio","state":"running",`+
							`"started_at":"2026-11-22T00:35:27.881140082Z","executable":"/usr/bin/minio",`+
							`"args":["server","/data","--console-address",":9001"],"bindings":[{"HostIp":"127.0.0.1","HostPort":"19000"}],"mounts":%s}`,
							containerID, verifiedLocalMinIOImageID, scenario.mounts)
						return []byte(row + "\n"), nil
					case "diff":
						if !slices.Equal(arguments, []string{"container", "diff", containerID}) {
							t.Fatalf("检查镜像覆盖时没有使用同一 ID，得到 %q", arguments)
						}
						diffCalls++
						return []byte(scenario.changes), nil
					default:
						t.Fatalf("意外的容器命令 %q", arguments)
						return nil, nil
					}
				})
			}
			verify := newLocalMinIOSessionLockVerifier("fixture", read)
			identity, err := verify(t.Context(), localMinIOFixtureEndpoint)
			if err != nil || (identity != "") != scenario.wantProof || (diffCalls == 1) != scenario.wantDiff {
				t.Fatal("镜像覆盖检查结果不合预期", identity, diffCalls, err)
			}
		})
	}
}

func TestReadLocalMinIODockerSnapshotsRejectsIncompleteRead(t *testing.T) {
	containerID := strings.Repeat("ab", 32)
	for _, scenario := range []struct {
		name       string
		ids, rows  string
		inspectErr error
	}{
		{name: "short container ID", ids: containerID[:12]},
		{name: "container removed after listing", ids: containerID, inspectErr: errors.New("fixture private detail")},
		{name: "partial output", ids: containerID + "\n" + strings.Repeat("cd", 32), rows: `{}`},
		{name: "unrelated record", ids: containerID, rows: `{"id":"other"}`},
		{name: "malformed output", ids: containerID, rows: `truncated`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshots, err := readLocalMinIODockerSnapshots(t.Context(), "fixture", func(_ context.Context, arguments ...string) ([]byte, error) {
				if arguments[1] == "ls" {
					return []byte(scenario.ids), nil
				}
				return []byte(scenario.rows), scenario.inspectErr
			})
			if snapshots != nil || err != domain.ErrUploadStorageUnavailable {
				t.Fatal("不完整读取应返回固定错误和空结果", snapshots, err)
			}
		})
	}
}
