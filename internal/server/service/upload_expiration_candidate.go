package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadExpirationCandidateProcessor 串联一次单文件过期推进和收尾。
type UploadExpirationCandidateProcessor struct {
	begin    func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error)
	complete func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
}

// NewUploadExpirationCandidateProcessor 绑定两段可独立提交的事务。
func NewUploadExpirationCandidateProcessor(
	begin func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error),
	complete func(context.Context, *domain.UploadTask) (*domain.UploadTask, error),
) *UploadExpirationCandidateProcessor {
	// 1. 校验构造依赖，保留推进与收尾入口。
	if begin == nil || complete == nil {
		panic("处理到期上传候选需要阶段推进和收尾方法")
	}
	return &UploadExpirationCandidateProcessor{begin: begin, complete: complete}
}

// Process 根据单个到期候选返回已终结的单文件任务。
func (processor *UploadExpirationCandidateProcessor) Process(ctx context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
	// 1. 核对候选是否属于单文件上传。
	if candidate.UploadMode != "SINGLE" {
		return nil, domain.ErrUploadTaskNotReady
	}

	// 2. 复核并推进到 EXPIRING，取得已提交的任务。
	expiring, err := processor.begin(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if expiring == nil || expiring.Status != domain.UploadStatusExpiring || expiring.UploadMode != candidate.UploadMode ||
		expiring.ID != candidate.TaskID || expiring.UserID != candidate.UserID || expiring.StagedObjectID != candidate.StagedObjectID ||
		expiring.LeaseVersion != candidate.LeaseVersion || !expiring.ExpiresAt.Equal(candidate.ExpiresAt) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 执行过期收尾，返回终态或错误。
	return processor.complete(ctx, expiring)
}
