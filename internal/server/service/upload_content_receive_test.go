package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type uploadReceiveTransactionStub struct {
	advance func(context.Context, uint64, uint64, uint64, string, domain.UploadObjectResult) (*domain.UploadTask, error)
	renew   func(context.Context, uint64, uint64, uint64, string) (*domain.UploadTask, error)
}

func (stub uploadReceiveTransactionStub) BeginUserUploadVerification(ctx context.Context, taskID, objectID, version uint64, owner string, confirmation domain.UploadObjectResult) (*domain.UploadTask, error) {
	return stub.advance(ctx, taskID, objectID, version, owner, confirmation)
}

func (stub uploadReceiveTransactionStub) RenewUserUploadContentLease(ctx context.Context, taskID, objectID, version uint64, owner string) (*domain.UploadTask, error) {
	return stub.renew(ctx, taskID, objectID, version, owner)
}

func TestUploadContentReceiveService(t *testing.T) {
	commitFailure := errors.New("fixture commit failure")
	for _, fixture := range []struct {
		name string
		want error
		stop string
	}{
		{"success with new object", nil, "commit"},
		{"foreign declaration", domain.ErrInvalidUploadTaskData, ""},
		{"preparation busy", domain.ErrUploadTaskBusy, "prepare"},
		{"size changed", domain.ErrInvalidUploadTaskData, "prepare"},
		{"digest removed", domain.ErrInvalidUploadTaskData, "prepare"},
		{"digest changed", domain.ErrInvalidUploadTaskData, "prepare"},
		{"missing lease", domain.ErrInvalidUploadTaskData, "prepare"},
		{"wrong provider", domain.ErrInvalidStoredObjectData, "prepare"},
		{"wrong bucket", domain.ErrInvalidStoredObjectData, "prepare"},
		{"wrong object", domain.ErrInvalidStoredObjectData, "prepare"},
		{"short body", domain.ErrUploadBodySizeMismatch, "read"},
		{"wrong checksum", domain.ErrUploadChecksumMismatch, "read"},
		{"object exists", domain.ErrUploadObjectExists, "put"},
		{"invalid confirmation", domain.ErrUploadStorageUnavailable, "put"},
		{"cancel after put", context.Canceled, "put"},
		{"lease lost before advance", domain.ErrUploadTaskBusy, "advance"},
		{"invalid advance", domain.ErrInvalidUploadTaskData, "advance"},
		{"commit failure", commitFailure, "commit"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			directory := t.TempDir()
			space := NewUploadTempSpace()
			files, err := NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			actor := domain.Actor{UserID: 7, DeviceID: 12, SessionID: 17}
			now := time.Now().UTC()
			hash := sha256.Sum256([]byte("abc"))
			declaration := &domain.UploadTask{
				ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13, UploadMode: "SINGLE",
				Status: domain.UploadStatusUploading, FileSize: 3, ReservedBytes: 3, ExpectedSHA256: &hash,
				UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
			}
			owner, until := "receive-attempt", now.Add(time.Minute)
			task := copyUploadTask(declaration)
			task.StagedObjectID, task.LeaseVersion = 25, 2
			task.LeaseOwner, task.LeaseUntil = &owner, &until
			object := &domain.StoredObject{
				ID: 25, OwnerUserID: 7, Status: domain.StoredObjectStatusStaging,
				Provider: "local_minio", Bucket: "test-bucket", ObjectKey: "new-attempt", FileSize: 3,
			}
			payload := "abc"
			switch fixture.name {
			case "foreign declaration":
				declaration.UserID++
			case "size changed":
				task.FileSize, task.ReservedBytes, object.FileSize = 4, 4, 4
			case "digest removed":
				task.ExpectedSHA256 = nil
			case "digest changed":
				*task.ExpectedSHA256 = sha256.Sum256([]byte("xyz"))
			case "missing lease":
				task.LeaseVersion = 0
			case "wrong provider":
				object.Provider = "other"
			case "wrong bucket":
				object.Bucket = "other"
			case "wrong object":
				object.ID++
			case "short body":
				payload = "ab"
			case "wrong checksum":
				payload = "xyz"
			}
			var events []string
			body := bytes.NewBufferString(payload)
			firstRead := true
			source := uploadCopyReadFunc(func(buffer []byte) (int, error) {
				if firstRead {
					events = append(events, "read")
					firstRead = false
				}
				return body.Read(buffer)
			})
			etag := "fixture-etag"
			var receiveContext context.Context
			prepare := func(prepareContext context.Context, current domain.Actor, taskID uint64) (*domain.UploadContentPreparation, error) {
				events = append(events, "prepare")
				receiveContext = prepareContext
				if prepareContext == ctx || prepareContext.Err() != nil || current != actor || taskID != declaration.ID || space.activeReceivers != 1 || space.reservedBytes != 3 {
					t.Fatal("准备未收到当前身份、任务或资源预占")
				}
				if fixture.name == "preparation busy" {
					return nil, fixture.want
				}
				return &domain.UploadContentPreparation{Task: task, Object: object}, nil
			}
			putObject := func(putContext context.Context, key string, file io.ReadSeeker, size uint64) (*domain.UploadObjectResult, error) {
				events = append(events, "put")
				data, err := io.ReadAll(file)
				if err != nil || putContext != receiveContext || key != "new-attempt" || string(data) != "abc" || size != 3 {
					t.Fatal("写入未使用本次暂存位置和已校验文件", err)
				}
				switch fixture.name {
				case "object exists":
					return nil, fixture.want
				case "invalid confirmation":
					return &domain.UploadObjectResult{SizeBytes: 2}, nil
				case "cancel after put":
					cancel()
				}
				return &domain.UploadObjectResult{SizeBytes: size, StorageETag: &etag}, nil
			}
			withTransaction := func(transactionContext context.Context, current domain.Actor, action func(context.Context, UploadContentReceiveTransaction) error) error {
				if transactionContext != receiveContext || current != actor {
					t.Fatal("推进未使用请求上下文和身份")
				}
				txContext, stop := context.WithCancel(transactionContext)
				defer stop()
				advance := func(callContext context.Context, taskID, objectID, version uint64, leaseOwner string, confirmation domain.UploadObjectResult) (*domain.UploadTask, error) {
					events = append(events, "advance")
					if callContext != txContext || taskID != 19 || objectID != 25 || version != 2 || leaseOwner != owner || confirmation.SizeBytes != 3 || confirmation.StorageETag == nil || *confirmation.StorageETag != etag {
						t.Fatal("推进未使用当前租约或对象确认")
					}
					if fixture.name == "lease lost before advance" {
						return nil, fixture.want
					}
					advanced := copyUploadTask(task)
					advanced.LeaseOwner, advanced.LeaseUntil = nil, nil
					if fixture.name != "invalid advance" {
						advanced.Status = domain.UploadStatusVerifying
					}
					return advanced, nil
				}
				err := action(txContext, uploadReceiveTransactionStub{advance: advance})
				if err != nil {
					return err
				}
				events = append(events, "commit")
				if fixture.name == "commit failure" {
					return commitFailure
				}
				return nil
			}
			receiver := NewUploadContentReceiveService(files, "local_minio", "test-bucket", prepare, putObject, withTransaction)
			result, err := receiver.Receive(ctx, actor, declaration, source)
			if !errors.Is(err, fixture.want) {
				t.Fatal("结果错误", err)
			}
			if fixture.want != nil && result != nil {
				t.Fatal("失败时交付了任务")
			}
			if fixture.want == nil && (result == nil || result.Status != domain.UploadStatusVerifying || result.StagedObjectID != 25 || result.ExpectedSHA256 == task.ExpectedSHA256) {
				t.Fatal("未交付独立的校验阶段任务")
			}
			order := []string{"prepare", "read", "put", "advance", "commit"}
			if !slices.Equal(events, order[:slices.Index(order, fixture.stop)+1]) {
				t.Fatal("执行顺序或失败短路错误", events)
			}
			entries, readError := os.ReadDir(directory)
			if readError != nil || len(entries) != 0 || space.activeReceivers != 0 || space.reservedBytes != 0 || len(files.pending) != 0 {
				t.Fatal("临时文件或接收资源未回收", readError)
			}
		})
	}
}
