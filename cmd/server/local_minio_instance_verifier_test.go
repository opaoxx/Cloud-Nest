package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const localMinIOFixtureEndpoint = "http://127.0.0.1:19000"

func localMinIOFixtureSnapshot() localMinIOInstanceSnapshot {
	return localMinIOInstanceSnapshot{
		ContainerID: strings.Repeat("ab", 32), ImageID: "sha256:7c840b89c21e0af0e824cf57310becadbbd45c7fb407c78cd2ada4b74c76f733",
		ComposeProject: "cloudnest", ComposeService: "minio", State: "running",
		StartedAt:  time.Date(2026, 11, 22, 0, 35, 27, 881140082, time.UTC),
		Executable: "/usr/bin/minio", Arguments: []string{"server", "/data", "--console-address", ":9001"},
		S3Bindings: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:19000")},
	}
}

func TestLocalMinIOSessionLockVerifierReadsFreshIdentity(t *testing.T) {
	// 1. 同一端点允许部署里的其他服务和已退出的旧容器共存。
	current := localMinIOFixtureSnapshot()
	old := current
	old.State = "exited"
	other := current
	other.ComposeProject = "other"
	reads := 0
	ctx := t.Context()
	verify := newLocalMinIOSessionLockVerifier("cloudnest", func(received context.Context) ([]localMinIOInstanceSnapshot, error) {
		if received != ctx {
			t.Error("读取快照没有沿用调用方上下文")
		}
		reads++
		return []localMinIOInstanceSnapshot{old, other, current}, nil
	})
	check := func(want string) {
		t.Helper()
		identity, err := verify(ctx, localMinIOFixtureEndpoint)
		if err != nil || identity != want {
			t.Fatalf("运行身份不符，得到 %q，期望 %q，错误 %v", identity, want, err)
		}
	}
	const originalStart = "2026-11-22T00:35:27.881140082Z"
	firstID := current.ContainerID
	check(firstID + "/" + originalStart)
	current.StartedAt = current.StartedAt.In(time.FixedZone("fixture UTC+8", 8*60*60))
	check(firstID + "/" + originalStart)

	// 2. 同一端点的重启更新启动时间，替换更新容器 ID，两次都重新读取事实。
	current.StartedAt = current.StartedAt.Add(time.Nanosecond)
	const restartedStart = "2026-11-22T00:35:27.881140083Z"
	check(firstID + "/" + restartedStart)
	current.ContainerID = strings.Repeat("cd", 32)
	check(current.ContainerID + "/" + restartedStart)
	if reads != 4 {
		t.Fatal("没有按每次核对重新读取快照", reads)
	}
}

func TestLocalMinIOSessionLockVerifierRejectsUnverifiedSnapshots(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*localMinIOInstanceSnapshot)
	}{
		{"wrong compose project", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ComposeProject = "other" }},
		{"wrong compose service", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ComposeService = "mysql" }},
		{"missing compose service", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ComposeService = "" }},
		{"not running", func(snapshot *localMinIOInstanceSnapshot) { snapshot.State = "restarting" }},
		{"paused", func(snapshot *localMinIOInstanceSnapshot) { snapshot.State = "paused" }},
		{"image tag only", func(snapshot *localMinIOInstanceSnapshot) {
			snapshot.ImageID = "cloudnest/minio:RELEASE.2025-10-15T17-29-55Z-source"
		}},
		{"different image", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ImageID = "sha256:" + strings.Repeat("ab", 32) }},
		{"missing image ID", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ImageID = "" }},
		{"short ID", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ContainerID = snapshot.ContainerID[:12] }},
		{"malformed ID", func(snapshot *localMinIOInstanceSnapshot) { snapshot.ContainerID = strings.Repeat("z", 64) }},
		{"missing start time", func(snapshot *localMinIOInstanceSnapshot) { snapshot.StartedAt = time.Time{} }},
		{"different executable", func(snapshot *localMinIOInstanceSnapshot) { snapshot.Executable = "/bin/sh" }},
		{"different startup args", func(snapshot *localMinIOInstanceSnapshot) { snapshot.Arguments[1] = "http://other-host/data" }},
		{"missing bindings", func(snapshot *localMinIOInstanceSnapshot) { snapshot.S3Bindings = nil }},
		{"wildcard binding", func(snapshot *localMinIOInstanceSnapshot) {
			snapshot.S3Bindings[0] = netip.MustParseAddrPort("0.0.0.0:19000")
		}},
		{"wrong binding port", func(snapshot *localMinIOInstanceSnapshot) {
			snapshot.S3Bindings[0] = netip.MustParseAddrPort("127.0.0.1:19001")
		}},
		{"multiple bindings", func(snapshot *localMinIOInstanceSnapshot) {
			snapshot.S3Bindings = append(snapshot.S3Bindings, snapshot.S3Bindings[0])
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshot := localMinIOFixtureSnapshot()
			scenario.change(&snapshot)
			verify := newLocalMinIOSessionLockVerifier("cloudnest", func(context.Context) ([]localMinIOInstanceSnapshot, error) {
				return []localMinIOInstanceSnapshot{snapshot}, nil
			})
			if identity, err := verify(t.Context(), localMinIOFixtureEndpoint); identity != "" || err != nil {
				t.Fatalf("不满足条件时仍交付证明或错误，得到 %q，错误 %v", identity, err)
			}
		})
	}

	// 第二个运行实例即使镜像不同，也不能凭排序选中第一个。
	first := localMinIOFixtureSnapshot()
	second := first
	second.ContainerID = strings.Repeat("cd", 32)
	second.ImageID = "sha256:" + strings.Repeat("aa", 32)
	for _, instances := range [][]localMinIOInstanceSnapshot{nil, {first, second}, {second, first}} {
		verify := newLocalMinIOSessionLockVerifier("cloudnest", func(context.Context) ([]localMinIOInstanceSnapshot, error) {
			return instances, nil
		})
		if identity, err := verify(t.Context(), localMinIOFixtureEndpoint); identity != "" || err != nil {
			t.Fatal("空快照或多实例仍交付证明或错误", identity, err)
		}
	}
}

func TestLocalMinIOSessionLockVerifierRejectsUnsupportedEndpoints(t *testing.T) {
	verify := newLocalMinIOSessionLockVerifier("cloudnest", func(context.Context) ([]localMinIOInstanceSnapshot, error) {
		t.Fatal("不受支持的端点触发了部署查询")
		return nil, nil
	})
	for _, endpoint := range []string{
		"", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1:019000",
		"https://127.0.0.1:19000", "http://localhost:19000", "http://127.0.0.2:19000", "http://[::1]:19000",
		"http://192.0.2.1:19000", "http://127.0.0.1:19000/", "http://127.0.0.1:19000?source=fixture",
	} {
		if identity, err := verify(t.Context(), endpoint); identity != "" || err != nil {
			t.Fatalf("端点 %q 不符合约定，得到 %q，错误 %v", endpoint, identity, err)
		}
	}
}

func TestLocalMinIOSessionLockVerifierHandlesUnavailableSnapshots(t *testing.T) {
	localError := errors.New("fixture read failure")
	reads := 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	verify := newLocalMinIOSessionLockVerifier("cloudnest", func(context.Context) ([]localMinIOInstanceSnapshot, error) {
		reads++
		if reads == 1 {
			return []localMinIOInstanceSnapshot{localMinIOFixtureSnapshot()}, localError
		}
		cancel()
		return []localMinIOInstanceSnapshot{localMinIOFixtureSnapshot()}, nil
	})
	for _, want := range []error{localError, context.Canceled, context.Canceled} {
		if identity, err := verify(ctx, localMinIOFixtureEndpoint); identity != "" || !errors.Is(err, want) {
			t.Fatalf("读取错误或取消时得到 %q，错误 %v，期望 %v", identity, err, want)
		}
	}
	if reads != 2 {
		t.Fatal("调用方已取消时仍读取快照", reads)
	}
	withoutSource := newLocalMinIOSessionLockVerifier("cloudnest", nil)
	if identity, err := withoutSource(t.Context(), localMinIOFixtureEndpoint); identity != "" || err != nil {
		t.Fatal("缺少受信来源时仍交付证明或错误", identity, err)
	}
	if identity, err := withoutSource(nil, localMinIOFixtureEndpoint); identity != "" || !errors.Is(err, domain.ErrUploadStorageUnavailable) {
		t.Fatal("空上下文没有拒绝证明", identity, err)
	}
}
