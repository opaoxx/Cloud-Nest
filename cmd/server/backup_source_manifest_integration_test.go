package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

type backupManifestEntry struct {
	ID        uint64     `json:"id,string"`
	UserID    uint64     `json:"user_id,string"`
	ParentID  *uint64    `json:"parent_id,string"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Revision  uint64     `json:"revision,string"`
	DeletedAt *time.Time `json:"deleted_at"`
}

type backupManifestFile struct {
	backupManifestEntry
	ObjectID  uint64 `json:"object_id,string"`
	SizeBytes uint64 `json:"size_bytes"`
}

type backupManifestObject struct {
	ID          uint64  `json:"id,string"`
	UserID      uint64  `json:"user_id,string"`
	Provider    string  `json:"provider"`
	Bucket      string  `json:"bucket"`
	Key         string  `json:"object_key"`
	VersionID   *string `json:"storage_version_id"`
	SizeBytes   uint64  `json:"size_bytes"`
	SHA256      string  `json:"sha256"`
	PayloadPath string  `json:"payload_path"`
}

type backupSourceManifest struct {
	DatabaseSHA256 string                 `json:"database_sha256"`
	Directories    []backupManifestEntry  `json:"directories"`
	FileReferences []backupManifestFile   `json:"file_references"`
	Objects        []backupManifestObject `json:"objects"`
}

func TestServerBackupSourceManifestIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后生成隔离源端清单")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	verifyBackupSourceBaseline(t, ctx, fixture)
	backupDir := t.TempDir()
	dump := stopAndExportBackupSourceSQL(t, ctx, fixture, backupDir)
	writeAndVerifyBackupSourceManifest(t, ctx, fixture, backupDir, dump)
}

func writeAndVerifyBackupSourceManifest(t *testing.T, ctx context.Context, fixture backupSourceFixture, backupDir string, dump []byte) backupSourceManifest {
	t.Helper()
	// 1. 使用同一次停写得到的 SQL 摘要和目录与对象记录。
	databaseSHA256 := fmt.Sprintf("%x", sha256.Sum256(dump))
	manifest, err := readBackupSourceManifest(ctx, fixture.pool, fixture.bucket.name, databaseSHA256)
	if err != nil {
		t.Fatal("生成源端清单失败", err)
	}

	// 2. 生成临时 JSON，读回实际文件并校验快照绑定、目录和对象定位。
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(backupDir, "manifest.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(saved, payload) {
		t.Fatal("临时清单读回失败", err)
	}
	var decoded backupSourceManifest
	if err := json.Unmarshal(saved, &decoded); err != nil || decoded.DatabaseSHA256 != databaseSHA256 {
		t.Fatal("清单未绑定本轮 SQL 快照", err)
	}
	verifyBackupSourceManifest(t, ctx, fixture, decoded)
	t.Logf("清单 %d B，SHA-256 %x，绑定 SQL SHA-256 %s，包含 %d 个目录、%d 条文件引用及 %d 个对象版本",
		len(saved), sha256.Sum256(saved), decoded.DatabaseSHA256, len(decoded.Directories), len(decoded.FileReferences), len(decoded.Objects))
	return decoded
}

func readBackupSourceManifest(ctx context.Context, pool *sql.DB, bucket, databaseSHA256 string) (backupSourceManifest, error) {
	// 1. 在一个只读查询里保留目录及所有未永久清理的文件，包括回收目录内的活动子项。
	rows, err := pool.QueryContext(ctx, `SELECT e.id,e.user_id,e.parent_id,e.name,e.entry_type,e.status,e.revision,e.deleted_at,
		e.object_id,e.file_size,o.status,o.provider,o.bucket,o.object_key,o.storage_version_id,o.file_size,LOWER(HEX(o.verified_sha256))
		FROM file_entries e LEFT JOIN stored_objects o ON o.owner_user_id=e.user_id AND o.id=e.object_id
		WHERE e.status<>'PURGED' ORDER BY e.user_id,e.id`)
	if err != nil {
		return backupSourceManifest{}, err
	}
	defer rows.Close()
	manifest := backupSourceManifest{
		DatabaseSHA256: databaseSHA256, Directories: []backupManifestEntry{},
		FileReferences: []backupManifestFile{}, Objects: []backupManifestObject{},
	}
	seenObjects := make(map[uint64]bool)
	for rows.Next() {
		var entry backupManifestEntry
		var entryType string
		var entryBytes uint64
		var objectID, objectBytes sql.Null[uint64]
		var objectStatus, provider, storedBucket, key, version, digest sql.NullString
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.ParentID, &entry.Name, &entryType, &entry.Status, &entry.Revision, &entry.DeletedAt,
			&objectID, &entryBytes, &objectStatus, &provider, &storedBucket, &key, &version, &objectBytes, &digest); err != nil {
			return backupSourceManifest{}, err
		}
		if entryType == "DIR" {
			manifest.Directories = append(manifest.Directories, entry)
			continue
		}

		// 2. LEFT JOIN 保留缺失引用。必须先验证同归属的 READY 对象和定位，才能登记文件。
		hash, hashErr := hex.DecodeString(digest.String)
		if entryType != "FILE" || !objectID.Valid || objectID.V == 0 || objectStatus.String != "READY" ||
			provider.String != "local_minio" || storedBucket.String != bucket || key.String == "" ||
			(version.Valid && version.String == "") || !objectBytes.Valid || objectBytes.V != entryBytes ||
			hashErr != nil || len(hash) != sha256.Size {
			return backupSourceManifest{}, fmt.Errorf("文件 %d 的对象定位、状态、大小或摘要不完整", entry.ID)
		}
		manifest.FileReferences = append(manifest.FileReferences, backupManifestFile{
			backupManifestEntry: entry, ObjectID: objectID.V, SizeBytes: entryBytes,
		})

		// 3. 同一个物理对象只登记一次。使用对象 ID 生成正文相对路径。
		if !seenObjects[objectID.V] {
			object := backupManifestObject{ID: objectID.V, UserID: entry.UserID, Provider: provider.String, Bucket: storedBucket.String,
				Key: key.String, SizeBytes: objectBytes.V, SHA256: digest.String, PayloadPath: fmt.Sprintf("objects/%d.bin", objectID.V)}
			if version.Valid {
				object.VersionID = &version.String
			}
			manifest.Objects = append(manifest.Objects, object)
			seenObjects[object.ID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return backupSourceManifest{}, err
	}
	return manifest, nil
}

func verifyBackupSourceManifest(t *testing.T, ctx context.Context, fixture backupSourceFixture, manifest backupSourceManifest) {
	t.Helper()
	// 1. 核对目录树和文件引用。回收目录子项保留自身状态，仍指向原父目录。
	if len(manifest.Directories) != 2 || len(manifest.FileReferences) != 3 || len(manifest.Objects) != 3 {
		t.Fatalf("清单条目数不符，目录=%d 文件引用=%d 对象=%d", len(manifest.Directories), len(manifest.FileReferences), len(manifest.Objects))
	}
	userID := backupFixtureID(t, fixture.login.User.ID)
	rootID := backupFixtureID(t, fixture.login.RootEntryID)
	root, folder := manifest.Directories[0], manifest.Directories[1]
	if root.ID != rootID || root.UserID != userID || root.ParentID != nil || root.Name != "" || root.Status != "ACTIVE" ||
		folder.ID == rootID || folder.UserID != userID || folder.ParentID == nil || *folder.ParentID != rootID ||
		folder.Name != "隔离备份回收目录" || folder.Status != "TRASHED" || folder.DeletedAt == nil {
		t.Fatal("清单没有保留根目录、回收目录和回收时间")
	}
	objects := make(map[uint64]backupManifestObject)
	for _, object := range manifest.Objects {
		objects[object.ID] = object
	}
	want := map[string]struct {
		parentID uint64
		body     []byte
	}{
		"活动文件.bin":    {rootID, []byte{0, 42, 128, 255}},
		"回收目录子文件.bin": {folder.ID, []byte{1, 2, 3, 4, 5, 6}},
		"空文件.bin":     {rootID, nil},
	}
	for _, file := range manifest.FileReferences {
		sample, ok := want[file.Name]
		object := objects[file.ObjectID]
		if !ok || file.UserID != userID || file.ParentID == nil || *file.ParentID != sample.parentID || file.Status != "ACTIVE" ||
			file.SizeBytes != uint64(len(sample.body)) || object.UserID != file.UserID || object.SizeBytes != file.SizeBytes ||
			object.SHA256 != fmt.Sprintf("%x", sha256.Sum256(sample.body)) {
			t.Fatal("清单的文件父目录、归属、大小或摘要不符", file.ID, file.Name)
		}
		delete(want, file.Name)
	}

	// 2. 只读列举版本，确保所有列出的正文都能按桶、键、版本和大小定位。
	byKey := make(map[string]backupManifestObject)
	for _, object := range manifest.Objects {
		if object.Provider != "local_minio" || object.Bucket != fixture.bucket.name || object.Key == "" ||
			object.VersionID == nil || *object.VersionID == "" || *object.VersionID == "null" ||
			object.PayloadPath != fmt.Sprintf("objects/%d.bin", object.ID) {
			t.Fatal("清单的对象定位或正文路径不符", object.ID)
		}
		byKey[object.Key] = object
	}
	for info := range fixture.bucket.client.ListObjects(ctx, fixture.bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		object, ok := byKey[info.Key]
		if info.Err != nil || !ok || info.IsDeleteMarker || info.Size < 0 || uint64(info.Size) != object.SizeBytes || info.VersionID != *object.VersionID {
			t.Fatal("隔离 MinIO 版本与清单不符", info.Key, info.Err)
		}
		delete(byKey, info.Key)
	}
	if len(byKey) != 0 {
		t.Fatal("清单中有未找到的对象版本")
	}
}
