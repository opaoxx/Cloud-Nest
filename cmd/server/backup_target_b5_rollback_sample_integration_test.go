package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

type backupTargetB5RollbackSample struct {
	fixture  backupTargetRemapFixture
	manifest backupSourceManifest
	state    backupB5DatabaseState
}

func TestServerBackupTargetB5RollbackSampleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后准备 B5 回退演练样本")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	sample := prepareBackupTargetB5RollbackSample(t, ctx)
	current := sample.manifest.FileReferences[0]
	t.Logf("B5 回退样本已停写，条目 %d 当前 v%d %d B、历史 v1 和 v2，保留事件 1～%d，已用 %d B，预占 %d B",
		current.ID, current.Revision, current.SizeBytes, sample.state.high, sample.state.used, sample.state.reserved)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", sample.fixture.source.mysqlContainer, sample.fixture.source.minioContainer)
}

func prepareBackupTargetB5RollbackSample(t *testing.T, ctx context.Context) backupTargetB5RollbackSample {
	t.Helper()
	// 1. 恢复隔离目标，记录 v2、历史 v1、配额和日志。
	fixture := prepareBackupTargetB5Remap(t, ctx)
	verifyAndApplyBackupTargetB5ObjectMap(t, ctx, fixture)
	resetBackupTargetB5SyncDevices(t, ctx, fixture)
	if os.Getenv("CLOUDNEST_S3_BUCKET") != fixture.targetBucket.name {
		t.Fatal("替换对象的目的桶未绑定本轮隔离目标")
	}
	pool := fixture.database.pool
	target, stopTarget := startBackupFixtureServer(t, ctx, fixture.database.name)
	login := loginBackupFixture(t, ctx, &target, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if login.User.ID != fixture.source.login.User.ID || login.RootEntryID != fixture.source.login.RootEntryID ||
		login.Session.ID == fixture.source.login.Session.ID {
		t.Fatal("恢复目标未建立原合成账号的独立会话")
	}
	userID := backupFixtureID(t, login.User.ID)
	before := readBackupB5DatabaseState(t, ctx, pool, userID)
	original, err := readBackupSourceManifest(ctx, pool, fixture.targetBucket.name, "")
	if err != nil || len(original.FileReferences) != 1 || len(original.HistoryReferences) != 1 || len(original.Objects) != 2 ||
		before.high != 4 || len(before.events) != 4 || before.used != 72 || before.reserved != 0 {
		t.Fatal("恢复目标的切换基线与 B5 样本不符", err)
	}
	file := original.FileReferences[0]
	if file.UserID != userID || file.ParentID == nil || fmt.Sprint(*file.ParentID) != login.RootEntryID ||
		file.Revision != 2 || original.HistoryReferences[0].Revision != 1 {
		t.Fatal("恢复目标缺少同一文件的 v2 当前引用与 v1 历史引用")
	}

	// 2. 用正式接口提交 v3，等校验发布完成，再关停服务。
	replacement := []byte("cloudnest rollback sample after cutover\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(replacement))
	var created dto.CreateUploadResponse
	target.request(t, ctx, http.MethodPost, fmt.Sprintf("/api/v1/entries/%d/content-replacement", file.ID), "b5-target-after-cutover-v3",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":"2","size_bytes":%d,"sha256":%q}`, len(replacement), digest)), http.StatusCreated, &created)
	task := created.Data.Task
	if created.RequestID == "" || task.UploadMode != "SINGLE" || task.Status != "UPLOADING" ||
		task.SizeBytes != uint64(len(replacement)) || created.Data.ContentPath != "/api/v1/uploads/"+task.ID+"/content" ||
		task.Replacement == nil || task.Replacement.EntryID != fmt.Sprint(file.ID) || task.Replacement.ExpectedRevision != "2" {
		t.Fatal("正式入口未创建绑定当前 v2 的单次替换任务")
	}
	reservation := readBackupB5DatabaseState(t, ctx, pool, userID)
	if reservation.used != before.used || reservation.reserved != task.SizeBytes || reservation.high != before.high {
		t.Fatal("提交正文前应只新增本次替换的全额预占")
	}
	sendSingleReplacementContent(t, ctx, target, created.Data, replacement)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	waitContext, stopWait := context.WithTimeout(ctx, 25*time.Second)
	defer stopWait()
	published := awaitServerUploadPublication(t, waitContext, store, userID, backupFixtureID(t, task.ID))
	if published.ResultEntryID == nil || *published.ResultEntryID != file.ID || published.PublishedObjectID == nil ||
		published.ReplacementPublishedRevision == nil || *published.ReplacementPublishedRevision != 3 || published.ReservedBytes != 0 {
		t.Fatal("切换后替换未在同一文件发布 v3 并结算预占")
	}
	logs, err := stopTarget()
	if err != nil {
		t.Fatal("新写入发布后隔离目标关停失败", err)
	}
	verifyBackupB5ServiceStopped(t, target.baseURL, logs)

	// 3. 在停写状态核对三份正文、历史引用、配额和新增事件。
	manifest := verifyBackupTargetB5RollbackVersions(t, ctx, fixture, original, *published.PublishedObjectID, replacement, digest)
	after := readBackupB5DatabaseState(t, ctx, pool, userID)
	if after.used != before.used+task.SizeBytes || after.reserved != 0 || after.high != 5 || len(after.events) != 5 ||
		!reflect.DeepEqual(after.events[:4], before.events) || !reflect.DeepEqual(after.devices, before.devices) {
		t.Fatal("停写目标应只增加已用空间和一条事件，并保留原设备状态")
	}
	event := after.events[4]
	wantPayload := map[string]any{"entry_id": fmt.Sprint(file.ID), "parent_id": login.RootEntryID, "name": file.Name,
		"revision": "3", "size_bytes": float64(len(replacement)), "sha256": digest}
	var payload map[string]any
	if event.sequence != 5 || event.entryID != file.ID || event.revision != 3 || event.eventType != domain.FileContentReplacedEventType ||
		json.Unmarshal([]byte(event.payload), &payload) != nil || !reflect.DeepEqual(payload, wantPayload) {
		t.Fatal("新增序号 5 未完整记录 v3 的内容替换事实")
	}
	return backupTargetB5RollbackSample{fixture: fixture, manifest: manifest, state: after}
}

func verifyBackupTargetB5RollbackVersions(t *testing.T, ctx context.Context, fixture backupTargetRemapFixture,
	original backupSourceManifest, publishedObjectID uint64, replacement []byte, digest string,
) backupSourceManifest {
	t.Helper()
	// 1. 核对目录、文件身份和两个历史引用，保留原 v1 的完整记录。
	manifest, err := readBackupSourceManifest(ctx, fixture.database.pool, fixture.targetBucket.name, "")
	if err != nil || !reflect.DeepEqual(manifest.Directories, original.Directories) || len(manifest.FileReferences) != 1 ||
		len(manifest.HistoryReferences) != 2 || len(manifest.Objects) != 3 {
		t.Fatal("停写样本应包含一个当前文件、两条历史引用和三个物理对象", err)
	}
	expected := original.FileReferences[0]
	previous := expected
	expected.Revision, expected.ObjectID, expected.SizeBytes = 3, publishedObjectID, uint64(len(replacement))
	if !reflect.DeepEqual(manifest.FileReferences[0], expected) ||
		!reflect.DeepEqual(manifest.HistoryReferences[0], original.HistoryReferences[0]) {
		t.Fatal("v3 未保留原文件身份或历史 v1 记录发生变化")
	}
	v2 := manifest.HistoryReferences[1]
	if v2.ID == original.HistoryReferences[0].ID || v2.EntryID != previous.ID || v2.UserID != previous.UserID ||
		v2.Revision != previous.Revision || v2.ObjectID != previous.ObjectID || v2.SizeBytes != previous.SizeBytes ||
		!v2.RetainedUntil.Equal(v2.CreatedAt.AddDate(0, 0, domain.DefaultFileVersionRetentionDays)) {
		t.Fatal("旧 v2 未转为按默认保留期计费的历史引用")
	}

	// 2. 按对象定位读取三个实际版本，通过大小、摘要和磁盘读回核对正文。
	current := manifest.Objects[0]
	// 清单按引用顺序排列，原当前 v2 转入第二条历史后排在 v1 之后。
	previousObjects := []backupManifestObject{original.Objects[1], original.Objects[0]}
	if current.ID != publishedObjectID || current.SHA256 != digest || current.SizeBytes != expected.SizeBytes ||
		!reflect.DeepEqual(manifest.Objects[1:], previousObjects) {
		t.Fatal("v3 物理定位不符，或原有两个对象版本发生变化")
	}
	verifyBackupManifestObjectVersions(t, ctx, fixture.targetBucket, manifest.Objects)
	root := openBackupSourceBundleRoot(t, t.TempDir())
	for _, object := range manifest.Objects {
		if err := saveBackupObjectPayload(ctx, fixture.targetBucket, root, object); err != nil {
			t.Fatal("未能按数据库记录读回合成对象正文", object.ID, err)
		}
		t.Logf("B5 回退样本对象 %d，目标版本 %s，%d B，SHA-256 %s；已按版本读回",
			object.ID, *object.VersionID, object.SizeBytes, object.SHA256)
	}
	if body, err := root.ReadFile(current.PayloadPath); err != nil || !bytes.Equal(body, replacement) {
		t.Fatal("停写样本的 v3 正文与提交的合成字节不一致", err)
	}
	return manifest
}
