package service

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadContentReceiveServiceRenewal(t *testing.T) {
	commitFailure := errors.New("fixture renewal commit failure")
	for _, fixture := range []struct {
		name  string
		want  error
		order []string
	}{
		{"wait for renewal commit", nil, []string{"prepare", "read", "renew", "put", "renew committed", "verify"}},
		{"lost lease during read", domain.ErrUploadTaskBusy, []string{"prepare", "read", "renew"}},
		{"lost lease during storage write", domain.ErrUploadTaskBusy, []string{"prepare", "read", "put", "renew"}},
		{"renewal commit fails while stopping", commitFailure, []string{"prepare", "read", "renew", "put", "renew commit failed"}},
		{"panic cancels active renewal", nil, []string{"prepare", "read", "put", "renew", "renew canceled"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				space := NewUploadTempSpace()
				files, err := NewUploadContentFileService(directory, space)
				if err != nil {
					t.Fatal(err)
				}
				actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
				now := time.Now().UTC()
				owner, until := "renewable-attempt", now.Add(time.Minute)
				declaration := &domain.UploadTask{
					ID: 19, UserID: 7, DeviceID: 11, TargetParentID: 13, UploadMode: "SINGLE",
					StagedObjectID: 23, Status: domain.UploadStatusUploading, FileSize: 3, ReservedBytes: 3,
					UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
				}
				task := copyUploadTask(declaration)
				task.StagedObjectID, task.LeaseVersion, task.LeaseOwner, task.LeaseUntil = 25, 2, &owner, &until
				object := &domain.StoredObject{
					ID: 25, OwnerUserID: 7, Status: domain.StoredObjectStatusStaging,
					Provider: "local_minio", Bucket: "test-bucket", ObjectKey: "new-attempt", FileSize: 3,
				}
				var events []string
				var eventMutex sync.Mutex
				record := func(event string) {
					eventMutex.Lock()
					defer eventMutex.Unlock()
					events = append(events, event)
				}
				slowRead := fixture.name != "lost lease during storage write" && fixture.name != "panic cancels active renewal"
				body := strings.NewReader("abc")
				firstRead := true
				source := uploadCopyReadFunc(func(buffer []byte) (int, error) {
					if firstRead {
						firstRead = false
						record("read")
						if slowRead {
							time.Sleep(21 * time.Second)
						}
					}
					return body.Read(buffer)
				})
				var receiveContext context.Context
				prepare := func(ctx context.Context, _ domain.Actor, _ uint64) (*domain.UploadContentPreparation, error) {
					receiveContext = ctx
					record("prepare")
					return &domain.UploadContentPreparation{Task: task, Object: object}, nil
				}
				put := func(ctx context.Context, key string, _ io.ReadSeeker, size uint64) (*domain.UploadObjectResult, error) {
					record("put")
					if ctx != receiveContext || key != object.ObjectKey {
						t.Error("写入未沿用当前接收上下文或对象")
					}
					switch fixture.name {
					case "lost lease during storage write":
						<-ctx.Done()
						return nil, ctx.Err()
					case "panic cancels active renewal":
						time.Sleep(21 * time.Second)
						panic("fixture put panic")
					}
					return &domain.UploadObjectResult{SizeBytes: size}, nil
				}
				withTransaction := func(ctx context.Context, current domain.Actor, action func(context.Context, UploadContentReceiveTransaction) error) error {
					if ctx != receiveContext || current != actor {
						t.Error("事务未沿用接收上下文或认证身份")
					}
					txContext, end := context.WithCancel(ctx)
					defer end()
					renewCalled := false
					stub := uploadReceiveTransactionStub{
						renew: func(callContext context.Context, taskID, objectID, version uint64, leaseOwner string) (*domain.UploadTask, error) {
							record("renew")
							renewCalled = true
							if callContext != txContext || taskID != 19 || objectID != 25 || version != 2 || leaseOwner != owner {
								t.Error("续租未使用本次已提交的租约")
							}
							switch fixture.name {
							case "lost lease during read", "lost lease during storage write":
								return nil, domain.ErrUploadTaskBusy
							case "panic cancels active renewal":
								<-callContext.Done()
								record("renew canceled")
								return nil, callContext.Err()
							}
							return task, nil
						},
						advance: func(context.Context, uint64, uint64, uint64, string, domain.UploadObjectResult) (*domain.UploadTask, error) {
							record("verify")
							advanced := copyUploadTask(task)
							advanced.Status, advanced.LeaseOwner, advanced.LeaseUntil = domain.UploadStatusVerifying, nil, nil
							return advanced, nil
						},
					}
					if err := action(txContext, stub); err != nil || !renewCalled {
						return err
					}
					// 续租已经写入，模拟事务提交尚未完成。
					time.Sleep(10 * time.Second)
					if fixture.name == "renewal commit fails while stopping" {
						record("renew commit failed")
						return commitFailure
					}
					record("renew committed")
					return nil
				}
				receiver := NewUploadContentReceiveService(files, "local_minio", "test-bucket", prepare, put, withTransaction)
				var result *domain.UploadTask
				var recovered any
				func() {
					defer func() { recovered = recover() }()
					result, err = receiver.Receive(parent, actor, declaration, source)
				}()
				if fixture.name == "panic cancels active renewal" {
					if recovered != "fixture put panic" {
						t.Fatal("存储 panic 未原样传播", recovered)
					}
				} else if recovered != nil || !errors.Is(err, fixture.want) || (result != nil) != (fixture.want == nil) {
					t.Fatal("接收结果丢失或提前推进校验", result, err, recovered)
				}
				if parent.Err() != nil || receiveContext.Err() != context.Canceled {
					t.Fatal("接收结束应释放子上下文，保留父上下文")
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if !slices.Equal(events, fixture.order) {
					t.Fatal("接收、续租与校验顺序错误", events)
				}
				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != 0 || space.activeReceivers != 0 || space.reservedBytes != 0 {
					t.Fatal("接收退出后仍有未释放临时资源", err)
				}
			})
		})
	}
}
