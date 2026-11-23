package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerB5LocalReeditIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证 B5 写回前的本地再次编辑")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 确认原文件基线，记录本地扫描摘要。
	fixture := prepareBackupSource(t, ctx)
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	const fileName = "reedit.txt"
	const stagedName = "incoming-revision-2.tmp"
	original := []byte("baseline specimen v1\n")
	remote := []byte("incoming specimen v2\n")
	localEdit := []byte("re-edits specimen v3\n")
	localEditDigest, remoteDigest := sha256.Sum256(localEdit), sha256.Sum256(remote)
	entryID, originalObject := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, userID, rootID, 1, fileName, original)
	entryIDText := fmt.Sprint(entryID)
	entryPath := "/api/v1/entries/" + entryIDText
	local, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error("关闭本地合成目录失败", err)
		}
	})
	if err := local.WriteFile(fileName, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := establishB5DeletionTestBaseline(t, ctx, fixture.api, local, fixture.login.RootEntryID, 1)
	scanned, err := local.ReadFile(fileName)
	if err != nil || !bytes.Equal(scanned, original) {
		t.Fatal("扫描应读到已确认的本地基线正文", err)
	}
	scanDigest := sha256.Sum256(scanned)
	if len(scanned) != len(remote) || len(scanned) != len(localEdit) ||
		scanDigest == remoteDigest || scanDigest == localEditDigest || remoteDigest == localEditDigest {
		t.Fatal("三份合成正文应等长且摘要各异")
	}

	// 2. 拉取远端替换事件，把签名下载的正文写到临时文件。
	otherAPI := fixture.api
	otherLogin := loginBackupFixture(t, ctx, &otherAPI, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if otherLogin.User.ID != fixture.login.User.ID || otherLogin.Device.ID == "" || otherLogin.Device.ID == fixture.login.Device.ID {
		t.Fatal("替换应由同一合成账号的另一设备执行")
	}
	var created dto.CreateUploadResponse
	otherAPI.request(t, ctx, http.MethodPost, entryPath+"/content-replacement", "b5-reedit-remote-replacement",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":"1","size_bytes":%d,"sha256":"%x"}`, len(remote), remoteDigest)), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	if created.Data.ContentPath != "/api/v1/uploads/"+created.Data.Task.ID+"/content" {
		t.Fatal("替换任务没有交付自己的 SINGLE 正文地址")
	}
	sendSingleReplacementContent(t, ctx, otherAPI, created.Data, remote)
	waitContext, stopWait := context.WithTimeout(ctx, 25*time.Second)
	defer stopWait()
	published := awaitServerUploadPublication(t, waitContext, store, userID, taskID)
	if published.ResultEntryID == nil || *published.ResultEntryID != entryID ||
		published.ReplacementPublishedRevision == nil || *published.ReplacementPublishedRevision != 2 ||
		published.PublishedObjectID == nil || *published.PublishedObjectID == originalObject.ID {
		t.Fatal("替换应为同一条目提交修订 2 和新的已校验对象")
	}
	remoteObject, err := store.FindUserStoredObject(ctx, userID, *published.PublishedObjectID)
	if err != nil || remoteObject == nil {
		t.Fatal("已发布的远端对象无法定位", err)
	}
	var changes dto.PullSyncChangesResponse
	fixture.api.request(t, ctx, http.MethodPost, "/api/v1/sync/changes/pull", "",
		strings.NewReader(fmt.Sprintf(`{"baseline_token":%q}`, *snapshot.BaselineToken)), http.StatusOK, &changes)
	if changes.RequestID == "" || changes.Data.HighWaterSequence != "2" || changes.Data.HasMore || changes.Data.NextCursor == "" ||
		len(changes.Data.Events) != 1 || changes.Data.Events[0].EventType != domain.FileContentReplacedEventType ||
		changes.Data.Events[0].Sequence != "2" || changes.Data.Events[0].EntryID != entryIDText || changes.Data.Events[0].Revision != "2" {
		t.Fatal("基线后的唯一增量应指向该条目的远端修订 2")
	}
	func() {
		staged, err := local.OpenFile(stagedName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal("无法创建本轮的下载临时文件", err)
		}
		defer func() {
			if err := staged.Close(); err != nil {
				t.Error("关闭本轮下载临时文件失败", err)
			}
		}()
		copyVerifiedReplacementDownload(t, ctx, fixture, entryPath, 2, remoteObject, remote, staged)
	}()
	before := readBackupB5DatabaseState(t, ctx, fixture.pool, userID)
	if before.high != 2 || len(before.events) != 2 || before.events[1].eventType != domain.FileContentReplacedEventType ||
		len(before.devices) != 1 || before.devices[0].DeviceID != backupFixtureID(t, fixture.login.Device.ID) ||
		before.devices[0].State.AckSequence != 1 || before.used != uint64(len(original)+len(remote)) || before.reserved != 0 {
		t.Fatal("远端修改已发布，原设备应仍保留基线确认位置 1")
	}

	// 3. 写回前重读本地，发现摘要变化就保留两份正文。
	if err := local.WriteFile(fileName, localEdit, 0o600); err != nil {
		t.Fatal("无法在测试目录模拟扫描后的编辑", err)
	}
	applied, err := applyB5TestDownloadIfUnchanged(local, fileName, stagedName, scanDigest)
	if err != nil || applied {
		t.Fatal("扫描后发生编辑时应推迟写回", applied, err)
	}
	localBody, localErr := local.ReadFile(fileName)
	stagedBody, stagedErr := local.ReadFile(stagedName)
	if localErr != nil || stagedErr != nil || !bytes.Equal(localBody, localEdit) || !bytes.Equal(stagedBody, remote) {
		t.Fatal("本地原路径和暂存文件应分别保留两份完整正文", localErr, stagedErr)
	}
	verifyReplacementDownload(t, ctx, fixture, entryPath, 2, remoteObject, remote)
	if after := readBackupB5DatabaseState(t, ctx, fixture.pool, userID); !reflect.DeepEqual(after, before) {
		t.Fatal("推迟写回应保留原设备确认位置和远端业务状态")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离同步服务关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, fixture.api.baseURL, logs)
	t.Logf("B5 写回前发现本地再次编辑，保留本地 %d B SHA-256 %x 与远端修订 2 %d B SHA-256 %x，确认位置 1",
		len(localBody), sha256.Sum256(localBody), len(stagedBody), sha256.Sum256(stagedBody))
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", fixture.mysqlContainer, fixture.minioContainer)
}

func applyB5TestDownloadIfUnchanged(local *os.Root, name, stagedName string, scanDigest [sha256.Size]byte) (bool, error) {
	// 1. 用原路径当前正文的摘要复核扫描结果，若已改动则保留两份文件。
	current, err := local.ReadFile(name)
	if err != nil {
		return false, err
	}
	if sha256.Sum256(current) != scanDigest {
		return false, nil
	}
	// 2. 摘要匹配时才把已校验的暂存文件移到原路径。
	if err := local.Rename(stagedName, name); err != nil {
		return false, err
	}
	return true, nil
}
