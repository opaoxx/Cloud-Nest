package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadVerificationCandidateProcessor 处理后台扫描返回的一条候选。
type UploadVerificationCandidateProcessor struct {
	// claimLease 提交领取事务后返回当前任务。
	claimLease func(context.Context, domain.UploadVerificationCandidate, string) (*domain.UploadTask, error)
	// findObject 按当前任务的账号与对象 ID 读取暂存对象。
	findObject func(context.Context, uint64, uint64) (*domain.StoredObject, error)
	// verify 执行一次读回校验及结果写回。
	verify func(context.Context, *domain.UploadTask, *domain.StoredObject) (*domain.UploadTask, error)
}

// NewUploadVerificationCandidateProcessor 绑定单个候选所需的三个入口。
func NewUploadVerificationCandidateProcessor(
	claimLease func(context.Context, domain.UploadVerificationCandidate, string) (*domain.UploadTask, error),
	findObject func(context.Context, uint64, uint64) (*domain.StoredObject, error),
	verify func(context.Context, *domain.UploadTask, *domain.StoredObject) (*domain.UploadTask, error),
) *UploadVerificationCandidateProcessor {
	// 1. 核对三个入口已装配，保存为单次处理所用的依赖。
	if claimLease == nil || findObject == nil || verify == nil {
		panic("处理上传校验候选需要租约领取、对象查询和校验方法")
	}
	return &UploadVerificationCandidateProcessor{claimLease: claimLease, findObject: findObject, verify: verify}
}

// Process 领取一条候选，返回已发布或已安排重试的任务，或本次处理的错误。
func (processor *UploadVerificationCandidateProcessor) Process(ctx context.Context, candidate domain.UploadVerificationCandidate, leaseOwner string) (*domain.UploadTask, error) {
	// 1. 按扫描候选领取租约，取得已提交的任务。
	task, err := processor.claimLease(ctx, candidate, leaseOwner)
	if err != nil {
		return nil, err
	}
	if task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID || task.StagedObjectID != candidate.StagedObjectID ||
		task.LeaseVersion == 0 || task.LeaseVersion-1 != candidate.LeaseVersion ||
		task.LeaseOwner == nil || *task.LeaseOwner != leaseOwner || task.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 按已领取任务的归属与引用读取暂存对象。
	staged, err := processor.findObject(ctx, task.UserID, task.StagedObjectID)
	if err != nil {
		return nil, err
	}
	if staged == nil || staged.ID != task.StagedObjectID || staged.OwnerUserID != task.UserID {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 执行一次读回校验，返回发布、重试或错误结果。
	return processor.verify(ctx, task, staged)
}
