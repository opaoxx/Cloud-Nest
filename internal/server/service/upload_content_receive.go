package service

import (
	"context"
	"fmt"
	"io"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadContentVerificationTransaction 提供接收完成后的事务内状态推进。
type UploadContentVerificationTransaction interface {
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
	withTransaction func(context.Context, domain.Actor, func(context.Context, UploadContentVerificationTransaction) error) error
}

// NewUploadContentReceiveService 组装接收依赖，provider 和 bucket 须来自写入客户端使用的配置。
func NewUploadContentReceiveService(
	files *UploadContentFileService,
	provider, bucket string,
	prepare func(context.Context, domain.Actor, uint64) (*domain.UploadContentPreparation, error),
	putObject func(context.Context, string, io.ReadSeeker, uint64) (*domain.UploadObjectResult, error),
	withTransaction func(context.Context, domain.Actor, func(context.Context, UploadContentVerificationTransaction) error) error,
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
	// 1. 核对认证身份和任务声明，进入临时文件接收流程。
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
	var objectID, leaseVersion uint64
	var objectKey, leaseOwner string
	var verifying *domain.UploadTask

	err := receiver.files.WithPreparedFile(ctx, source, declared.FileSize, declared.ExpectedSHA256, func(prepareContext context.Context) error {
		// 2. 领取租约，复核声明和暂存对象位置。
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
		return nil
	}, func(file io.ReadSeeker, digest domain.UploadContentDigest) error {
		// 3. 将校验后的临时文件写入当前对象。
		confirmation, err := receiver.putObject(ctx, objectKey, file, digest.SizeBytes)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if confirmation == nil || confirmation.SizeBytes != digest.SizeBytes {
			return domain.ErrUploadStorageUnavailable
		}

		// 4. 在用户事务中按本次租约推进 VERIFYING。
		return receiver.withTransaction(ctx, actor, func(txContext context.Context, txn UploadContentVerificationTransaction) error {
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

	// 5. 确认提交与临时文件处理结果，返回任务。
	if err != nil {
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
