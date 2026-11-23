package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestServerBackupSourceB5PayloadIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后保存 B5 隔离源端正文")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 从停写源端生成 SQL 和清单，在同一备份目录内准备正文路径。
	source := prepareBackupSource(t, ctx)
	backupDir := t.TempDir()
	manifest := prepareBackupSourceB5Manifest(t, ctx, source, backupDir)
	root := openBackupSourceBundleRoot(t, backupDir)

	// 2. 验证错误历史版本、大小和摘要的写入失败，再按清单保存两份正文。
	historicalID := manifest.HistoryReferences[0].ObjectID
	for _, object := range manifest.Objects {
		if object.ID == historicalID {
			verifyBackupPayloadRejections(t, ctx, source.bucket, root, object)
		}
	}
	saveAndVerifyBackupSourcePayloads(t, ctx, source.bucket, root, manifest.Objects)

	// 3. 复核同一目录中实际保存的 SQL、清单和两份正文。
	savedSQL, err := root.ReadFile("database.sql")
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(savedSQL)) != manifest.DatabaseSHA256 {
		t.Fatal("备份正文完成后的 SQL 与清单不符", err)
	}
	savedManifest, err := root.ReadFile("manifest.json")
	if err != nil {
		t.Fatal("读回备份清单失败", err)
	}
	var decoded backupSourceManifest
	if err := json.Unmarshal(savedManifest, &decoded); err != nil || !reflect.DeepEqual(decoded, manifest) {
		t.Fatal("备份正文完成后的清单与原记录不符", err)
	}
	t.Logf("B5 备份包含 SQL、清单及两份已读回的正文，清单 SHA-256 %x，绑定 SQL SHA-256 %s",
		sha256.Sum256(savedManifest), decoded.DatabaseSHA256)
	t.Logf("本轮专用容器 MySQL %s，MinIO %s，后端已关停", source.mysqlContainer, source.minioContainer)
}
