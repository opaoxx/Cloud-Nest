package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerReuseFileStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_REUSE_FILE_STARTUP_TEST") != "1" {
		t.Skip("显式启用后启动独占 MySQL、MinIO 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 启动专用容器及正式 run，通过真实上传发布合成源文件。
	fixture := prepareBackupSource(t, ctx)
	t.Logf("秒传专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	const sourceName = "合成秒传.bin"
	const path = "/api/v1/files/reuse"
	const key = "reuse-startup-fixture"
	payload := []byte{0, 19, 42, 128, 255}
	digest := sha256.Sum256(payload)
	userID, parentID := backupFixtureID(t, fixture.login.User.ID), backupFixtureID(t, fixture.login.RootEntryID)
	sourceID, object := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, userID, parentID, 1, sourceName, payload)
	body := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, sourceName, len(payload), digest)

	// 2. 验证正式路由先拒绝缺少凭证的请求，再交付首次创建与同键快照。
	anonymous := fixture.api
	anonymous.accessToken = ""
	var failure dto.ErrorResponse
	anonymous.request(t, ctx, http.MethodPost, path, key, strings.NewReader(body), http.StatusUnauthorized, &failure)
	if failure.Error.Code != "AUTH_REQUIRED" {
		t.Fatalf("正式入口未返回预期的认证错误，得到 %s", failure.Error.Code)
	}
	var first, replay dto.ReuseFileResponse
	fixture.api.request(t, ctx, http.MethodPost, path, key, strings.NewReader(body), http.StatusCreated, &first)
	fixture.api.request(t, ctx, http.MethodPost, path, key, strings.NewReader(body), http.StatusCreated, &replay)
	entryID := backupFixtureID(t, first.Data.Entry.ID)
	if entryID == sourceID || first.Data.Entry.ParentID != fixture.login.RootEntryID || first.Data.Entry.Name != "合成秒传 (1).bin" ||
		first.Data.Entry.EntryType != "FILE" || first.Data.Entry.Status != "ACTIVE" || first.Data.Entry.Revision != "1" ||
		first.Data.Entry.SizeBytes != uint64(len(payload)) || first.Data != replay.Data ||
		first.RequestID == "" || replay.RequestID == "" || first.RequestID == replay.RequestID {
		t.Fatal("首次创建和同键重放未交付预期快照")
	}

	// 3. 核对对象引用、配额和日志，同键重放只返回首次快照。
	content, err := store.FindUserFileEntryContent(ctx, userID, entryID)
	if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != object.ID {
		t.Fatal("正式入口未引用已发布的对象", err)
	}
	var used, reserved, changeSequence uint64
	var fileCount, objectCount, uploadCount, reuseCount, eventCount int
	if err := fixture.pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,change_seq,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=u.id AND entry_type='FILE'),
		(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=u.id),
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=u.id),
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=u.id AND operation=?),
		(SELECT COUNT(*) FROM change_events WHERE user_id=u.id)
		FROM users u WHERE u.id=?`, domain.FileReuseOperation, userID).
		Scan(&used, &reserved, &changeSequence, &fileCount, &objectCount, &uploadCount, &reuseCount, &eventCount); err != nil {
		t.Fatal(err)
	}
	if used != 2*uint64(len(payload)) || reserved != 0 || fileCount != 2 || objectCount != 1 || uploadCount != 1 || reuseCount != 1 ||
		changeSequence != 2 || eventCount != 2 {
		t.Fatalf("正式秒传账目、引用数或事件数不符，已用 %d、预占 %d、文件 %d、对象 %d、上传任务 %d、秒传记录 %d、序号 %d、事件 %d",
			used, reserved, fileCount, objectCount, uploadCount, reuseCount, changeSequence, eventCount)
	}
	var savedEntryID, revision, size uint64
	var eventType, payloadEntryID, payloadRevision, parent, name, sha256Hex string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT entry_id,revision,event_type,
		JSON_UNQUOTE(JSON_EXTRACT(payload,'$.entry_id')),JSON_UNQUOTE(JSON_EXTRACT(payload,'$.revision')),
		JSON_UNQUOTE(JSON_EXTRACT(payload,'$.parent_id')),JSON_UNQUOTE(JSON_EXTRACT(payload,'$.name')),
		JSON_EXTRACT(payload,'$.size_bytes'),JSON_UNQUOTE(JSON_EXTRACT(payload,'$.sha256'))
		FROM change_events AS events WHERE user_id=? AND events.sequence=2`, userID).
		Scan(&savedEntryID, &revision, &eventType, &payloadEntryID, &payloadRevision, &parent, &name, &size, &sha256Hex); err != nil ||
		savedEntryID != entryID || revision != 1 || eventType != "FILE_PUBLISHED" ||
		payloadEntryID != first.Data.Entry.ID || payloadRevision != first.Data.Entry.Revision ||
		parent != first.Data.Entry.ParentID || name != first.Data.Entry.Name || size != uint64(len(payload)) || sha256Hex != fmt.Sprintf("%x", digest) {
		t.Fatal("秒传事件与首次创建快照或已校验内容不符", err)
	}
	t.Logf("正式路由返回 401/201/201，条目 %d 和 %d 共用对象 %d，已用 %d B。秒传事件序号 %d，名称 %s，%d B，SHA-256 %s",
		sourceID, entryID, object.ID, used, changeSequence, name, size, sha256Hex)
}
