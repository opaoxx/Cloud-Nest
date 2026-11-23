package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerB5UnavailableSyncRootIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 同步根不可访问")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 发布两份合成文件，保存并确认基线。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	remoteRootID := backupFixtureID(t, fixture.login.RootEntryID)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	local, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error("关闭测试临时目录失败", err)
		}
	})
	const syncRootName = "sync-root"
	const parkedRootName = "temporarily-unavailable-root"
	if err := local.Mkdir(syncRootName, 0o700); err != nil {
		t.Fatal(err)
	}
	samples := []struct {
		name    string
		body    []byte
		entryID uint64
		object  *domain.StoredObject
	}{
		{name: "first.txt", body: []byte("first file survives missing sync root\n")},
		{name: "second.txt", body: []byte("second file survives missing sync root\n")},
	}
	var expectedUsed uint64
	for index := range samples {
		sample := &samples[index]
		sample.entryID, sample.object = publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket,
			userID, remoteRootID, index+1, sample.name, sample.body)
		if err := local.WriteFile(syncRootName+"/"+sample.name, sample.body, 0o600); err != nil {
			t.Fatal(err)
		}
		expectedUsed += uint64(len(sample.body))
	}
	snapshot := func() dto.SyncSnapshotPageData {
		root, err := local.OpenRoot(syncRootName)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := root.Close(); err != nil {
				t.Error("关闭基线同步根失败", err)
			}
		}()
		return establishB5DeletionTestBaseline(t, ctx, fixture.api, root, fixture.login.RootEntryID, len(samples))
	}()
	before := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if before.high != 2 || len(before.events) != 2 || before.events[0].eventType != domain.FilePublishedEventType ||
		before.events[1].eventType != domain.FilePublishedEventType ||
		len(before.devices) != 1 || before.devices[0].DeviceID != backupFixtureID(t, fixture.login.Device.ID) ||
		before.devices[0].State.Status != domain.SyncDeviceStatusActive || before.devices[0].State.AckSequence != 2 ||
		before.used != expectedUsed || before.reserved != 0 {
		t.Fatal("隔离服务尚未保存两份文件所在的已确认基线")
	}

	// 2. 临时移开测试同步根，核对打开失败时停止本轮传播。
	if err := local.Rename(syncRootName, parkedRootName); err != nil {
		t.Fatal("无法临时移开本轮测试同步根", err)
	}
	recorder := &b5DeletionRequestRecorder{transport: http.DefaultTransport}
	api := fixture.api
	api.httpClient = &http.Client{
		Transport: recorder, Timeout: fixture.api.httpClient.Timeout, CheckRedirect: fixture.api.httpClient.CheckRedirect,
	}
	client := b5DeletionTestClient{api: api, propagateLocalDeletes: true}
	if candidates, err := client.scanB5DeletionTestRoot(t, ctx, local, syncRootName, snapshot); !errors.Is(err, os.ErrNotExist) ||
		candidates != 0 || len(recorder.requests) != 0 {
		t.Fatal("临时不可访问的同步根应报告扫描错误并跳过所有删除判断", candidates, err, recorder.requests)
	}
	if after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID); !reflect.DeepEqual(after, before) {
		t.Fatal("同步根不可读期间应保留变更日志、设备位置和配额")
	}

	// 3. 恢复根目录，复查基线、同步位置和两端正文。
	if err := local.Rename(parkedRootName, syncRootName); err != nil {
		t.Fatal("无法还原本轮测试同步根", err)
	}
	baselineBytes, err := local.ReadFile(syncRootName + "/baseline.json")
	if err != nil {
		t.Fatal("同步根恢复后无法读回原基线", err)
	}
	var restored dto.SyncSnapshotPageData
	if err := json.Unmarshal(baselineBytes, &restored); err != nil || !reflect.DeepEqual(restored, snapshot) {
		t.Fatal("同步根恢复后应读回同一份已确认基线", err)
	}
	if candidates, err := client.scanB5DeletionTestRoot(t, ctx, local, syncRootName, restored); err != nil ||
		candidates != 0 || len(recorder.requests) != 0 {
		t.Fatal("同步根恢复后应能成功扫描，且没有删除候选或传播请求", candidates, err, recorder.requests)
	}
	var trash dto.TrashPageResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/trash", "", nil, http.StatusOK, &trash)
	if trash.RequestID == "" || len(trash.Data.Items) != 0 {
		t.Fatal("正式回收站应保持为空")
	}
	for _, sample := range samples {
		if retained, err := local.ReadFile(syncRootName + "/" + sample.name); err != nil || !bytes.Equal(retained, sample.body) {
			t.Fatal("本地合成正文无法完整读回", sample.name, err)
		}
		verifyReplacementDownload(t, ctx, fixture, fmt.Sprintf("/api/v1/entries/%d", sample.entryID), 1, sample.object, sample.body)
		t.Logf("原条目 %d 的修订 1 两端仍可读，%d B SHA-256 %x", sample.entryID, len(sample.body), sha256.Sum256(sample.body))
	}
	if after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID); !reflect.DeepEqual(after, before) {
		t.Fatal("恢复扫描及读取正文后，日志、设备位置和配额应与基线一致")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离同步服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	t.Logf("B5 同步根不可访问时暂停删除传播，恢复前后均无候选和传播请求，保留两份正文，确认位置 2，已用 %d B", expectedUsed)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func (client b5DeletionTestClient) scanB5DeletionTestRoot(t *testing.T, ctx context.Context,
	local *os.Root, syncRootName string, snapshot dto.SyncSnapshotPageData,
) (int, error) {
	t.Helper()
	// 1. 每轮先打开原同步根，失败就结束本轮，整棵树都没有可确认的扫描结果。
	root, err := local.OpenRoot(syncRootName)
	if err != nil {
		return 0, fmt.Errorf("打开同步根 %q: %w", syncRootName, err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error("关闭扫描同步根失败", err)
		}
	}()
	// 2. 根目录可读后，才在已确认基线上筛选缺项并执行条件复核。
	client.local = root
	missing := client.missingLocalEntries(t, snapshot)
	for _, entry := range missing {
		client.propagateLocalDeletion(t, ctx, entry)
	}
	return len(missing), nil
}
