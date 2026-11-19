package service

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadContentReceiveTransaction 提供接收期间续租以及完成后的状态推进。
type UploadContentReceiveTransaction interface {
	// RenewUserUploadContentLease 延长本次接收持有的租约。
	RenewUserUploadContentLease(context.Context, uint64, uint64, uint64, string) (*domain.UploadTask, error)
	// BeginUserUploadVerification 保存确认信息，按当前租约推进任务和对象。
	BeginUserUploadVerification(context.Context, uint64, uint64, uint64, string, domain.UploadObjectResult) (*domain.UploadTask, error)
}

// UploadContentReceiveService 组合租约准备、文件接收和校验阶段推进。
type UploadContentReceiveService struct {
	// files 管理共享接收名额、临时文件和容量。
	files *UploadContentFileService
	// prepare 返回已提交的当前租约和暂存对象。
	prepare func(context.Context, domain.Actor, uint64) (*domain.UploadContentPreparation, error)
	// putObject 绑定到 provider 和 bucket 对应的存储客户端。
	putObject        func(context.Context, string, io.ReadSeeker, uint64) (*domain.UploadObjectResult, error)
	provider, bucket string
	// withTransaction 仅在用户事务提交成功后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, UploadContentReceiveTransaction) error) error
}

// NewUploadContentReceiveService 组装接收依赖，provider 和 bucket 须来自写入客户端使用的配置。
func NewUploadContentReceiveService(
	files *UploadContentFileService,
	provider, bucket string,
	prepare func(context.Context, domain.Actor, uint64) (*domain.UploadContentPreparation, error),
	putObject func(context.Context, string, io.ReadSeeker, uint64) (*domain.UploadObjectResult, error),
	withTransaction func(context.Context, domain.Actor, func(context.Context, UploadContentReceiveTransaction) error) error,
) *UploadContentReceiveService {
	// 1. 核对依赖和写入客户端对应的位置。
	if files == nil || prepare == nil || putObject == nil || withTransaction == nil ||
		!(domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}).IsValid() {
		panic("上传内容接收服务需要文件服务、存储位置、准备、写入及用户事务")
	}
	// 2. 保存依赖供每次接收复用。
	return &UploadContentReceiveService{
		files: files, provider: provider, bucket: bucket, prepare: prepare,
		putObject: putObject, withTransaction: withTransaction,
	}
}

// Receive 使用已按用户读取的任务声明，接收正文并返回已提交的校验阶段任务。
// 正文由调用方持有。写入后若事务或临时文件清理失败，调用方可通过任务查询确认进度。
func (receiver *UploadContentReceiveService) Receive(ctx context.Context, actor domain.Actor, declaration *domain.UploadTask, source io.Reader) (*domain.UploadTask, error) {
	// 1. 校验任务声明，建立本次接收的可取消上下文。
	if ctx == nil || source == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("接收上传内容需要上下文、完整认证身份和正文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if declaration == nil || declaration.ID == 0 || declaration.UserID != actor.UserID {
		return nil, domain.ErrInvalidUploadTaskData
	}
	declared := copyUploadTask(declaration)
	receiveContext, cancelReceive := context.WithCancelCause(ctx)
	var stopRenewal func() error
	defer func() {
		cancelReceive(nil)
		if stopRenewal != nil {
			_ = stopRenewal()
		}
	}()
	var objectID, leaseVersion uint64
	var objectKey, leaseOwner string
	var verifying *domain.UploadTask

	err := receiver.files.WithPreparedFile(receiveContext, source, declared.FileSize, declared.ExpectedSHA256, func(prepareContext context.Context) error {
		// 2. 领取租约，核对对象后启动定时续租。
		prepared, err := receiver.prepare(prepareContext, actor, declared.ID)
		if err != nil {
			return err
		}
		if prepared == nil || prepared.Task == nil {
			return domain.ErrInvalidUploadTaskData
		}
		task, object := prepared.Task, prepared.Object
		if err := ValidateUploadContentState(task, actor.UserID, declared.ID, task.UpdatedAt); err != nil {
			return err
		}
		if !sameUploadContentDeclaration(task, declared) || task.LeaseOwner == nil || *task.LeaseOwner == "" ||
			task.LeaseVersion == 0 || task.LeaseUntil == nil || !task.LeaseUntil.After(task.UpdatedAt) {
			return domain.ErrInvalidUploadTaskData
		}
		if object == nil || object.ID != task.StagedObjectID || object.OwnerUserID != actor.UserID ||
			object.Status != domain.StoredObjectStatusStaging || object.FileSize != task.FileSize ||
			object.Provider != receiver.provider || object.Bucket != receiver.bucket ||
			!(domain.CreateStagedObjectInput{Provider: object.Provider, Bucket: object.Bucket, ObjectKey: object.ObjectKey}).IsValid() {
			return domain.ErrInvalidStoredObjectData
		}
		objectID, objectKey = object.ID, object.ObjectKey
		leaseVersion, leaseOwner = task.LeaseVersion, *task.LeaseOwner
		stopRenewal = startUploadContentLeaseRenewal(receiveContext, cancelReceive, func(renewContext context.Context) error {
			return receiver.withTransaction(renewContext, actor, func(txContext context.Context, txn UploadContentReceiveTransaction) error {
				_, err := txn.RenewUserUploadContentLease(txContext, declared.ID, objectID, leaseVersion, leaseOwner)
				return err
			})
		})
		return nil
	}, func(file io.ReadSeeker, digest domain.UploadContentDigest) error {
		// 3. 将校验后的临时文件写入当前对象，等待续租停止。
		confirmation, err := receiver.putObject(receiveContext, objectKey, file, digest.SizeBytes)
		if err != nil {
			return err
		}
		if confirmation == nil || confirmation.SizeBytes != digest.SizeBytes {
			return domain.ErrUploadStorageUnavailable
		}
		if err := stopRenewal(); err != nil {
			return err
		}

		// 4. 在用户事务中按本次租约推进 VERIFYING。
		return receiver.withTransaction(receiveContext, actor, func(txContext context.Context, txn UploadContentReceiveTransaction) error {
			task, err := txn.BeginUserUploadVerification(txContext, declared.ID, objectID, leaseVersion, leaseOwner, *confirmation)
			if err != nil {
				return err
			}
			if task == nil || task.ID != declared.ID || task.UserID != actor.UserID || task.StagedObjectID != objectID ||
				task.Status != domain.UploadStatusVerifying || task.LeaseVersion != leaseVersion || task.LeaseOwner != nil || task.LeaseUntil != nil ||
				!sameUploadContentDeclaration(task, declared) {
				return domain.ErrInvalidUploadTaskData
			}
			verifying = copyUploadTask(task)
			return nil
		})
	})

	// 5. 返回提交结果或错误，退出时统一清理续租循环。
	if err != nil {
		cancelReceive(err)
		if cause := context.Cause(receiveContext); cause != nil && !errors.Is(err, cause) {
			err = errors.Join(err, cause)
		}
		return nil, err
	}
	if verifying == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return verifying, nil
}

// sameUploadContentDeclaration 比较本次接收使用的大小和可空摘要声明。
func sameUploadContentDeclaration(left, right *domain.UploadTask) bool {
	if left.FileSize != right.FileSize || (left.ExpectedSHA256 == nil) != (right.ExpectedSHA256 == nil) {
		return false
	}
	return left.ExpectedSHA256 == nil || *left.ExpectedSHA256 == *right.ExpectedSHA256
}
