package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// maxUploadReplacementRequestSize 是替换声明允许的最大 JSON 安全整数。
const maxUploadReplacementRequestSize uint64 = 1<<53 - 1

// UploadReplacementDeclaration 保存规范化的目标、预期版本和新内容声明。
type UploadReplacementDeclaration struct {
	EntryID          uint64
	ExpectedRevision uint64
	SizeBytes        uint64
	ExpectedSHA256   [sha256.Size]byte
}

// UploadReplacementTargetLocker 在所属账号事务内锁读活动文件、祖先和预期版本。
type UploadReplacementTargetLocker interface {
	LockUploadReplacementTarget(ctx context.Context, entryID, expectedRevision uint64) (*domain.FileEntry, error)
}

// UploadReplacementPreparation 保存首次创建可在原事务中继续使用的声明、位置和上传计划。
type UploadReplacementPreparation struct {
	Declaration UploadReplacementDeclaration
	ParentID    uint64
	Name        string
	Plan        domain.UploadPlan
}

// PrepareUploadReplacement 只读准备一次首次替换创建。调用方须使用同一已认证账号事务完成后续写入。
func PrepareUploadReplacement(ctx context.Context, actor domain.Actor, declaration UploadReplacementDeclaration,
	limits domain.UploadPlanLimits, locker UploadReplacementTargetLocker,
) (*UploadReplacementPreparation, error) {
	// 1. 核对事务上下文、身份和替换声明。
	if err := validateUploadCreationContext(ctx, actor); err != nil {
		return nil, err
	}
	if locker == nil {
		return nil, fmt.Errorf("准备内容替换需要事务内的目标锁读能力")
	}
	if err := validateUploadReplacementDeclaration(declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes); err != nil {
		return nil, err
	}

	// 2. 锁读可见文件，取得父目录和原名称。
	entry, readError := locker.LockUploadReplacementTarget(ctx, declaration.EntryID, declaration.ExpectedRevision)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readError != nil {
		return nil, readError
	}
	if entry == nil || entry.ID != declaration.EntryID || entry.UserID != actor.UserID ||
		entry.Revision != declaration.ExpectedRevision || entry.Revision == math.MaxUint64 ||
		entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.ParentID == nil ||
		*entry.ParentID == 0 || *entry.ParentID == entry.ID || entry.Name == "" || !utf8.ValidString(entry.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 按新内容大小计算上传计划，返回只读准备结果。
	plan, err := domain.PlanUpload(declaration.SizeBytes, limits)
	if err != nil {
		return nil, err
	}
	return &UploadReplacementPreparation{
		Declaration: declaration,
		ParentID:    *entry.ParentID,
		Name:        entry.Name,
		Plan:        plan,
	}, nil
}

// validateUploadReplacementDeclaration 让请求摘要和首次准备共用声明范围。
func validateUploadReplacementDeclaration(entryID, expectedRevision, sizeBytes uint64) error {
	if entryID == 0 || expectedRevision == 0 {
		return fmt.Errorf("内容替换声明需要非零目标 ID 和预期版本")
	}
	if sizeBytes > maxUploadReplacementRequestSize {
		return fmt.Errorf("内容替换声明的文件大小超出 JSON 安全整数范围")
	}
	return nil
}
