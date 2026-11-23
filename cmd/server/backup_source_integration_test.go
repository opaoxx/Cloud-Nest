package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/migrations"
)

type backupSourceClient struct {
	baseURL     string
	accessToken string
	httpClient  *http.Client
}

type backupSourceFixture struct {
	api            backupSourceClient
	pool           *sql.DB
	bucket         *serverUploadContentBucket
	login          dto.LoginData
	databaseName   string
	mysqlContainer string
	minioContainer string
	stop           func() (string, error)
}

func TestServerBackupSourceIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后启动隔离备份源端")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	verifyBackupSourceBaseline(t, ctx, prepareBackupSource(t, ctx))
}

func verifyBackupSourceBaseline(t *testing.T, ctx context.Context, fixture backupSourceFixture) {
	t.Helper()
	// 1. 使用隔离账号在真实根目录下创建回收样本的父目录。
	source, pool, bucket, login := fixture.api, fixture.pool, fixture.bucket, fixture.login
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	userID := backupFixtureID(t, login.User.ID)
	rootID := backupFixtureID(t, login.RootEntryID)
	var folder dto.CreateDirectoryResponse
	source.request(t, ctx, http.MethodPost, "/api/v1/directories", "backup-source-directory",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":"隔离备份回收目录"}`, login.RootEntryID)), http.StatusCreated, &folder)
	folderEntry := folder.Data.Entry
	folderID := backupFixtureID(t, folderEntry.ID)

	// 2. 通过正式接口发布三个文件，再回收其中一个文件的父目录。
	files := []struct {
		name     string
		parentID uint64
		body     []byte
	}{
		{"活动文件.bin", rootID, []byte{0, 42, 128, 255}},
		{"回收目录子文件.bin", folderID, []byte{1, 2, 3, 4, 5, 6}},
		{"空文件.bin", rootID, nil},
	}
	entries := make([]string, 0, len(files))
	versions := make(map[string]string, len(files))
	var trashedChildID, trashedChildObjectID uint64
	for index, sample := range files {
		entryID, object := publishBackupSourceFile(t, ctx, source, store, bucket, userID, sample.parentID, index, sample.name, sample.body)
		entries = append(entries, strconv.FormatUint(entryID, 10))
		versions[object.ObjectKey] = *object.StorageVersionID
		if sample.parentID == folderID {
			trashedChildID, trashedChildObjectID = entryID, object.ID
		}
	}
	var trashed dto.TrashEntryResponse
	source.request(t, ctx, http.MethodPost, "/api/v1/entries/"+folderEntry.ID+"/trash", "backup-source-trash",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, folderEntry.Revision)), http.StatusOK, &trashed)
	var children dto.DirectoryPageResponse
	source.request(t, ctx, http.MethodGet, "/api/v1/entries?scope=directory&parent_id="+login.RootEntryID, "", nil, http.StatusOK, &children)
	active := make(map[string]uint64)
	for _, entry := range children.Data.Items {
		if entry.ParentID != login.RootEntryID || entry.Status != "ACTIVE" || entry.EntryType != "FILE" {
			t.Fatal("根目录包含意外条目", entry.ID)
		}
		active[entry.ID] = entry.SizeBytes
	}
	if len(active) != 2 || active[entries[0]] != 4 {
		t.Fatal("根目录没有列出预期的两个活动文件")
	}
	if size, ok := active[entries[2]]; !ok || size != 0 {
		t.Fatal("根目录没有列出空文件")
	}
	var trash dto.TrashPageResponse
	source.request(t, ctx, http.MethodGet, "/api/v1/trash", "", nil, http.StatusOK, &trash)
	if len(trash.Data.Items) != 1 || trash.Data.Items[0].ID != folderEntry.ID || trash.Data.Items[0].Status != "TRASHED" ||
		len(trash.Data.Items[0].OriginalPath) != 1 || trash.Data.Items[0].OriginalPath[0].ID != login.RootEntryID ||
		trashed.Data.Entry.DeletedAt.IsZero() || !trash.Data.Items[0].DeletedAt.Equal(trashed.Data.Entry.DeletedAt) {
		t.Fatal("回收站没有保留目录、原位置及回收时间")
	}
	var hidden dto.ErrorResponse
	source.request(t, ctx, http.MethodGet, "/api/v1/entries/"+entries[1], "", nil, http.StatusNotFound, &hidden)
	if hidden.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("回收目录的子项不应从常规入口可见", hidden.Error.Code)
	}
	var savedParent, savedObject uint64
	var childStatus string
	if err := pool.QueryRowContext(ctx, `SELECT parent_id,object_id,status FROM file_entries WHERE user_id=? AND id=?`,
		userID, trashedChildID).Scan(&savedParent, &savedObject, &childStatus); err != nil ||
		savedParent != folderID || savedObject != trashedChildObjectID || childStatus != "ACTIVE" {
		t.Fatal("回收后子项应保留原父目录、对象引用和自身活动状态", err)
	}

	// 3. 保留一项未上传任务，对照 MySQL、对象版本与公开配额建立基线。
	var pending dto.CreateUploadResponse
	source.request(t, ctx, http.MethodPost, "/api/v1/uploads", "backup-source-pending",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":"未发送.bin","size_bytes":3}`, login.RootEntryID)), http.StatusCreated, &pending)
	task, err := store.FindUserUploadTask(ctx, userID, backupFixtureID(t, pending.Data.Task.ID))
	if err != nil || task == nil || task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusUploading || task.ReservedBytes != 3 ||
		task.LeaseOwner != nil || task.LeaseUntil != nil || task.ResultEntryID != nil || task.PublishedObjectID != nil ||
		!task.ExpiresAt.After(time.Now()) {
		t.Fatal("未发送任务的状态、预占或租约不符", err)
	}
	staged, err := store.FindUserStoredObject(ctx, userID, task.StagedObjectID)
	if err != nil || staged == nil || staged.Status != domain.StoredObjectStatusStaging || staged.StorageVersionID != nil || staged.Bucket != bucket.name {
		t.Fatal("未发送任务应只持有源桶的暂存对象记录", err)
	}
	if len(versions) != len(files) {
		t.Fatal("三个固定样本应该对应三个不同的对象键")
	}
	for object := range bucket.client.ListObjects(ctx, bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		if object.Err != nil || object.IsDeleteMarker || versions[object.Key] != object.VersionID || object.VersionID == "" {
			t.Fatal("隔离桶包含清单之外的版本或标记", object.Err)
		}
		delete(versions, object.Key)
	}
	if len(versions) != 0 {
		t.Fatal("隔离桶缺少已发布的对象版本")
	}
	var used, reserved, logicalBytes, taskBytes uint64
	if err := pool.QueryRowContext(ctx, `SELECT u.used_storage,u.reserved_storage,
		(SELECT SUM(file_size) FROM file_entries WHERE user_id=u.id AND entry_type='FILE' AND status<>'PURGED'),
		(SELECT SUM(reserved_bytes) FROM upload_tasks WHERE user_id=u.id)
		FROM users u WHERE u.id=?`, userID).Scan(&used, &reserved, &logicalBytes, &taskBytes); err != nil {
		t.Fatal(err)
	}
	var profile dto.UserProfileResponse
	source.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if used != 10 || logicalBytes != used || reserved != 3 || taskBytes != reserved ||
		profile.Data.UsedBytes != used || profile.Data.ReservedBytes != reserved {
		t.Fatalf("额度基线不一致，库=%d/%d 汇总=%d/%d API=%d/%d", used, reserved, logicalBytes, taskBytes, profile.Data.UsedBytes, profile.Data.ReservedBytes)
	}
	t.Logf("源端基线：根目录 %d，回收目录 %d，未发送任务 %d，MinIO 共 3 个对象版本，已用 %d B，预占 %d B",
		rootID, folderID, task.ID, used, reserved)
}

func prepareBackupSource(t *testing.T, ctx context.Context) backupSourceFixture {
	t.Helper()
	return prepareBackupSourceWithMinIO(t, ctx, startIsolatedWebMinIO)
}

func prepareBackupSourceWithMinIO(t *testing.T, ctx context.Context, startMinIO func(*testing.T, context.Context) string) backupSourceFixture {
	t.Helper()
	return prepareBackupSourceWithServer(t, ctx, startMinIO, startBackupFixtureServer)
}

func prepareBackupSourceWithServer(t *testing.T, ctx context.Context, startMinIO func(*testing.T, context.Context) string,
	startServer func(*testing.T, context.Context, string) (backupSourceClient, func() (string, error))) backupSourceFixture {
	t.Helper()
	// 1. 独立启动 MySQL 与 MinIO，使用正式命令初始化空库和合成账号。
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startMinIO(t, ctx)
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	pool, databaseName := prepareEmptyServerIntegrationDatabase(t, ctx)
	password := rand.Text()
	t.Setenv("CLOUDNEST_ADMIN_PASSWORD", password)
	if output, err := exec.CommandContext(ctx, "go", "run", "../database", "-database", databaseName, "-seed").CombinedOutput(); err != nil {
		t.Fatalf("正式迁移命令失败，%v\n%s", err, output)
	}
	var ledgerRows int
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations.InitialSQL)))
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations
		WHERE version=1 AND name='000001_initial.up.sql' AND checksum=? AND dirty=false AND applied_at IS NOT NULL`, checksum).
		Scan(&ledgerRows); err != nil || ledgerRows != 1 {
		t.Fatal("正式迁移记录不完整", err)
	}
	bucket := prepareServerUploadContentBucket(t, ctx)

	// 2. 使用回环地址运行生产后端，退出时等待 HTTP 与后台任务关停。
	source, stop := startServer(t, ctx, databaseName)

	// 3. 登录正式接口取得令牌和根目录 ID，后续通过 HTTP 生成业务数据。
	login := loginBackupFixture(t, ctx, &source, password)
	if login.UsedBytes != 0 || login.ReservedBytes != 0 {
		t.Fatal("合成账号未从空配额开始")
	}
	t.Logf("隔离源端 MySQL 地址 %s、库 %s，MinIO 地址 %s、桶 %s，后端 %s，迁移 SHA-256 %s",
		os.Getenv("CLOUDNEST_DB_ADDRESS"), databaseName, os.Getenv("CLOUDNEST_S3_ENDPOINT"), bucket.name, source.baseURL, checksum)
	return backupSourceFixture{api: source, pool: pool, bucket: bucket, login: login, databaseName: databaseName,
		mysqlContainer: mysqlContainer, minioContainer: minioContainer, stop: stop}
}

func startBackupFixtureServer(t *testing.T, ctx context.Context, databaseName string) (backupSourceClient, func() (string, error)) {
	t.Helper()
	return startBackupFixtureServerWithRun(t, ctx, databaseName, run)
}

func startBackupFixtureServerWithRun(t *testing.T, ctx context.Context, databaseName string,
	runServer func(context.Context, *slog.Logger) error) (backupSourceClient, func() (string, error)) {
	t.Helper()
	return startConfiguredBackupFixtureServer(t, ctx, configureBackupFixtureServer(t, databaseName), runServer)
}

// startConfiguredBackupFixtureServer 启动已配置的隔离服务，供重启测试保留签名密钥。
func startConfiguredBackupFixtureServer(t *testing.T, ctx context.Context, address string,
	runServer func(context.Context, *slog.Logger) error) (backupSourceClient, func() (string, error)) {
	t.Helper()
	serverContext, cancelServer := context.WithCancel(ctx)
	done := make(chan struct{})
	var serverLogs bytes.Buffer
	var runError error
	go func() {
		defer close(done)
		runError = runServer(serverContext, logging.New(&serverLogs))
	}()
	stop := func() (string, error) {
		cancelServer()
		select {
		case <-done:
			return serverLogs.String(), runError
		case <-time.After(35 * time.Second):
			return "", fmt.Errorf("隔离备份服务未在期限内退出")
		}
	}
	t.Cleanup(func() {
		if _, err := stop(); err != nil {
			t.Error("隔离备份服务退出失败", err)
		}
	})
	return waitForBackupFixtureClient(t, ctx, address), stop
}

func configureBackupFixtureServer(t *testing.T, databaseName string) string {
	t.Helper()
	address := reserveWebLoginAddress(t)
	secret := sha256.Sum256([]byte(rand.Text()))
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(secret[:]))
	return address
}

func waitForBackupFixtureClient(t *testing.T, ctx context.Context, address string) backupSourceClient {
	t.Helper()
	waitForServerIntegrationHTTP(t, ctx, address)
	client := backupSourceClient{baseURL: "http://" + address, httpClient: &http.Client{
		Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	t.Cleanup(client.httpClient.CloseIdleConnections)
	return client
}

func loginBackupFixture(t *testing.T, ctx context.Context, client *backupSourceClient, password string) dto.LoginData {
	t.Helper()
	body, err := json.Marshal(dto.LoginRequest{Username: "root", Password: password, DeviceName: "隔离备份夹具",
		InstallationID: uuid.NewString(), ClientType: "native", Platform: "WINDOWS"})
	if err != nil {
		t.Fatal(err)
	}
	var login dto.LoginResponse
	client.request(t, ctx, http.MethodPost, "/api/v1/auth/login", "", bytes.NewReader(body), http.StatusOK, &login)
	if login.Data.AccessToken == "" {
		t.Fatal("合成账号登录没有返回访问令牌")
	}
	client.accessToken = login.Data.AccessToken
	return login.Data
}

func publishBackupSourceFile(t *testing.T, ctx context.Context, source backupSourceClient, store *repository.Store,
	bucket *serverUploadContentBucket, userID, parentID uint64, index int, name string, body []byte) (uint64, *domain.StoredObject) {
	t.Helper()
	// 1. 用固定合成字节生成声明，并把正文送到本次任务的接收地址。
	digest := sha256.Sum256(body)
	declaration := fmt.Sprintf(`{"parent_id":"%d","name":%q,"size_bytes":%d,"sha256":"%x"}`, parentID, name, len(body), digest)
	var created dto.CreateUploadResponse
	source.request(t, ctx, http.MethodPost, "/api/v1/uploads", fmt.Sprintf("backup-source-file-%d", index), strings.NewReader(declaration), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	if created.Data.ContentPath != "/api/v1/uploads/"+created.Data.Task.ID+"/content" {
		t.Fatal("上传回执未指向本次任务")
	}
	var accepted dto.UploadContentAcceptedResponse
	source.request(t, ctx, http.MethodPut, created.Data.ContentPath, "", bytes.NewReader(body), http.StatusAccepted, &accepted)
	if accepted.Data.TaskID != created.Data.Task.ID || accepted.Data.Status != "VERIFYING" {
		t.Fatal("正文接收未返回本次待校验任务")
	}

	// 2. 等后台发布，核对目录项与 MySQL 对象定位。
	waitContext, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	task := awaitServerUploadPublication(t, waitContext, store, userID, taskID)
	if task.ResultEntryID == nil || task.PublishedObjectID == nil || task.ReservedBytes != 0 {
		t.Fatal("发布任务没有文件、对象或已结算的预占")
	}
	entryID := *task.ResultEntryID
	entry, err := store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil || entry == nil || entry.ParentID == nil || *entry.ParentID != parentID || entry.Name != name ||
		entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.FileSize != uint64(len(body)) {
		t.Fatal("源端发布目录项不符", err)
	}
	content, err := store.FindUserFileEntryContent(ctx, userID, entryID)
	if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != *task.PublishedObjectID {
		t.Fatal("源端目录项没有引用已发布对象", err)
	}
	object, err := store.FindUserStoredObject(ctx, userID, *content.ObjectID)
	if err != nil || object == nil || object.Status != domain.StoredObjectStatusReady || object.Provider != "local_minio" ||
		object.Bucket != bucket.name || object.StorageVersionID == nil || *object.StorageVersionID == "null" ||
		object.FileSize != uint64(len(body)) || object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest {
		t.Fatal("源端数据库对象的定位、大小或摘要不符", err)
	}

	// 3. 从 MinIO 精确读取记录的版本，再按实际字节数和 SHA-256 复核。
	version := *object.StorageVersionID
	reader, err := bucket.client.GetObject(ctx, bucket.name, object.ObjectKey, minio.GetObjectOptions{VersionID: version})
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(io.LimitReader(reader, int64(len(body))+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(actual) != len(body) || sha256.Sum256(actual) != digest {
		t.Fatal("MinIO 源对象正文不符", name, readErr, closeErr)
	}
	t.Logf("源文件 %s，目录项 %d，父目录 %d，对象 %d，版本 %s，%d B，SHA-256 %x",
		name, entryID, parentID, object.ID, version, len(actual), digest)
	return entryID, object
}

func (source backupSourceClient) request(t *testing.T, ctx context.Context, method, path, key string, body io.Reader, status int, result any) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, source.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if source.accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+source.accessToken)
	}
	if path == "/api/v1/auth/login" {
		request.Header.Set("X-CloudNest-Client", "native")
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	} else if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := source.httpClient.Do(request)
	if err != nil {
		t.Fatal("隔离备份源请求失败", method, path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		var failure dto.ErrorResponse
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("%s %s 返回 %d，预期 %d，错误码 %s", method, path, response.StatusCode, status, failure.Error.Code)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal("隔离备份源响应格式错误", method, path, err)
	}
}

func backupFixtureID(t *testing.T, value string) uint64 {
	t.Helper()
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 {
		t.Fatal("隔离夹具收到无效 ID", value)
	}
	return id
}
