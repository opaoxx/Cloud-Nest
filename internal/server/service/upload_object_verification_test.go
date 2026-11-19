package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func uploadObjectVerificationFixture(size uint64) (*domain.UploadTask, *domain.StoredObject) {
	now := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	task := &domain.UploadTask{
		ID: 19, UserID: 7, StagedObjectID: 23, Status: domain.UploadStatusVerifying, UploadMode: "SINGLE",
		FileSize: size, LeaseVersion: 4, LeaseOwner: new("worker"), UpdatedAt: now, LeaseUntil: new(now.Add(time.Minute)),
	}
	object := &domain.StoredObject{ID: 23, OwnerUserID: 7, Status: domain.StoredObjectStatusVerifying, FileSize: size}
	return task, object
}

type uploadObjectVerificationStream struct {
	*uploadCopySource
	closeStream func() error
}

func (stream uploadObjectVerificationStream) Close() error {
	_ = stream.uploadCopySource.Close()
	return stream.closeStream()
}

func TestVerifyUploadObjectLifecycle(t *testing.T) {
	openError := domain.ErrUploadObjectMissing
	readError := domain.ErrUploadStorageUnavailable
	closeError := errors.New("fixture close failure")
	binary := []byte{0, 0xff, 0x80, 0x01}
	checksum := sha256.Sum256(binary)
	for _, fixture := range []struct {
		name      string
		want      []error
		wantClose int
		read      bool
	}{
		{"matching checksum", nil, 1, true},
		{"no checksum", nil, 1, true},
		{"snapshot before opening", nil, 1, true},
		{"checksum mismatch", []error{domain.ErrUploadChecksumMismatch}, 1, true},
		{"checksum mismatch and close failure", []error{domain.ErrUploadChecksumMismatch, closeError}, 1, true},
		{"checksum mismatch and cancellation", []error{domain.ErrUploadChecksumMismatch, context.Canceled}, 1, true},
		{"short object", []error{domain.ErrUploadBodySizeMismatch}, 1, true},
		{"open failed", []error{openError}, 0, false},
		{"empty open result", []error{domain.ErrUploadStorageUnavailable}, 0, false},
		{"open failed with stream", []error{openError, closeError}, 1, false},
		{"read and close fail", []error{readError, closeError}, 1, true},
		{"close failed after verification", []error{closeError}, 1, true},
		{"cancel after open", []error{context.Canceled}, 1, false},
		{"cancel during close", []error{context.Canceled}, 1, true},
		{"panic during read", nil, 1, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task, object := uploadObjectVerificationFixture(uint64(len(binary)))
			task.ExpectedSHA256 = new(checksum)
			var reader io.Reader = bytes.NewReader(binary)
			switch fixture.name {
			case "no checksum":
				task.ExpectedSHA256 = nil
			case "checksum mismatch", "checksum mismatch and close failure", "checksum mismatch and cancellation":
				task.ExpectedSHA256 = new([sha256.Size]byte{})
			case "short object":
				reader = bytes.NewReader(binary[:2])
			case "read and close fail":
				reader = iotest.ErrReader(readError)
			case "panic during read":
				reader = uploadCopyReadFunc(func([]byte) (int, error) { panic("fixture read panic") })
			}
			source := &uploadCopySource{reader: reader}
			stream := uploadObjectVerificationStream{uploadCopySource: source, closeStream: func() error {
				switch fixture.name {
				case "open failed with stream", "read and close fail", "close failed after verification", "checksum mismatch and close failure":
					return closeError
				case "cancel during close", "checksum mismatch and cancellation":
					cancel()
				}
				return nil
			}}
			opens := 0
			open := func(openContext context.Context, record *domain.StoredObject) (io.ReadCloser, error) {
				opens++
				if openContext != ctx || record != object {
					t.Error("读取未使用当前对象及上下文")
				}
				switch fixture.name {
				case "open failed":
					return nil, openError
				case "empty open result":
					return nil, nil
				case "open failed with stream":
					return stream, openError
				case "cancel after open":
					cancel()
				case "snapshot before opening":
					task.FileSize++
					*task.ExpectedSHA256 = [sha256.Size]byte{}
				}
				return stream, nil
			}
			var result *domain.UploadContentDigest
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				result, err = VerifyUploadObject(ctx, task, object, open)
			}()
			if opens != 1 || source.closes != fixture.wantClose || (source.reads > 0) != fixture.read {
				t.Fatal("打开、读取或关闭次数不符", opens, source.reads, source.closes)
			}
			if fixture.name == "panic during read" {
				if recovered != "fixture read panic" {
					t.Fatal("关闭对象流时未保留原 panic", recovered)
				}
				return
			}
			if recovered != nil {
				t.Fatal("意外 panic", recovered)
			}
			if len(fixture.want) == 0 {
				if err != nil || result == nil || result.SizeBytes != uint64(len(binary)) || result.SHA256 != checksum {
					t.Fatal("对象校验结果错误", result, err)
				}
				return
			}
			if result != nil {
				t.Fatal("失败仍返回校验结果", result)
			}
			for _, expected := range fixture.want {
				if !errors.Is(err, expected) {
					t.Fatal("未保留校验或关闭的失败原因", expected, err)
				}
			}
			mismatch, cleanMismatch := err.(*domain.UploadVerificationChecksumMismatch)
			if cleanMismatch != (fixture.name == "checksum mismatch") {
				t.Fatal("关闭失败或取消与纯摘要失配的处理不符", err)
			}
			if cleanMismatch && (mismatch.TaskID != task.ID || mismatch.UserID != task.UserID ||
				mismatch.StagedObjectID != object.ID || mismatch.LeaseVersion != task.LeaseVersion ||
				mismatch.ExpectedSHA256 != *task.ExpectedSHA256 || mismatch.Actual.SizeBytes != task.FileSize ||
				mismatch.Actual.SHA256 != checksum) {
				t.Fatal("摘要失配未绑定当前校验尝试")
			}
		})
	}
}

func TestVerifyUploadObjectRejectsInvalidRecords(t *testing.T) {
	for _, name := range []string{"missing task", "wrong task phase", "missing lease", "oversize", "missing object", "wrong object", "foreign owner", "wrong object state", "wrong object size", "canceled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task, object := uploadObjectVerificationFixture(4)
			want := domain.ErrInvalidStoredObjectData
			switch name {
			case "missing task":
				task, want = nil, domain.ErrInvalidUploadTaskData
			case "wrong task phase":
				task.Status, want = domain.UploadStatusUploading, domain.ErrUploadTaskNotReady
			case "missing lease":
				task.LeaseUntil, want = nil, domain.ErrInvalidUploadTaskData
			case "oversize":
				task.FileSize, want = domain.MaxSingleUploadSizeBytes+1, domain.ErrUploadFileTooLarge
			case "missing object":
				object = nil
			case "wrong object":
				object.ID++
			case "foreign owner":
				object.OwnerUserID++
			case "wrong object state":
				object.Status = domain.StoredObjectStatusReady
			case "wrong object size":
				object.FileSize++
			case "canceled":
				cancel()
				want = context.Canceled
			}
			result, err := VerifyUploadObject(ctx, task, object, func(context.Context, *domain.StoredObject) (io.ReadCloser, error) {
				t.Fatal("无效快照或取消仍打开了存储流")
				return nil, nil
			})
			if result != nil || !errors.Is(err, want) {
				t.Fatal("无效输入的错误类别不符", result, err)
			}
		})
	}
}
