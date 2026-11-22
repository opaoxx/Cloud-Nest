package main

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

const (
	multipartUploadSessionCleanupInterval = time.Minute
	// 以现有 24 小时上传任务生命周期作为首版孤儿会话宽限期。
	multipartUploadSessionCleanupGrace = 24 * time.Hour
)

// newServerMultipartUploadSessionCleanupRound 装配一个由 worker 调用的单轮收尾入口。
// 本轮和单项中止均要求可确认直接连接预期的存储实例。
func newServerMultipartUploadSessionCleanupRound(
	store *repository.Store, storageClient *storage.Client, settings config.StorageConfig,
	verifyInstance storage.MultipartSessionLockVerifier, limits service.MultipartUploadSessionCleanupLimits, gracePeriod time.Duration,
) (func(context.Context, service.MultipartUploadSessionCleanupPosition) (service.MultipartUploadSessionCleanupRoundResult, error), error) {
	// 1. 核对存储位置，装配使用相同 provider 和 bucket 的扫描及加锁复核。
	if store == nil || !storageClient.MatchesLocation(settings) {
		return nil, fmt.Errorf("分片会话收尾需要仓储和匹配的存储位置: %w", domain.ErrInvalidStoredObjectData)
	}
	endpoint := settings.EndpointURL.String()
	scanner := service.NewMultipartUploadSessionCandidateScanner(settings.Provider, settings.Bucket,
		storageClient.ListMultipartUploadSessionsPage, store.ReadMultipartUploadSessionCleanupFacts)
	batch := service.NewMultipartUploadSessionCandidateBatchScanner(scanner, store.ListMultipartUploadSessionScanTargets)
	rechecker := service.NewMultipartUploadSessionCandidateRechecker(settings.Provider, settings.Bucket,
		storageClient.CheckMultipartUploadSessionExists, store.RecheckMultipartUploadSessionCleanup)

	return func(ctx context.Context, position service.MultipartUploadSessionCleanupPosition) (service.MultipartUploadSessionCleanupRoundResult, error) {
		// 2. 固定本轮实例身份，在中止前及只读复查时核对它仍然相符。
		initial := service.MultipartUploadSessionCleanupRoundResult{Next: position}
		if ctx == nil {
			return initial, fmt.Errorf("分片会话单轮收尾需要上下文")
		}
		instance, err := verifyMultipartUploadSessionCleanupInstance(ctx, endpoint, verifyInstance)
		if err != nil {
			return initial, err
		}
		if instance == "" {
			return initial, domain.ErrUploadStorageUnavailable
		}
		verifySameInstance := func(callContext context.Context, address string) (string, error) {
			if address != endpoint {
				return "", nil
			}
			current, err := verifyMultipartUploadSessionCleanupInstance(callContext, address, verifyInstance)
			if err != nil || current != instance {
				return "", err
			}
			return current, nil
		}
		aborter := service.NewMultipartUploadSessionCandidateAborter(rechecker, func(callContext context.Context, objectKey, uploadID string) error {
			current, err := verifySameInstance(callContext, endpoint)
			if err != nil {
				return err
			}
			if current == "" {
				return domain.ErrUploadStorageUnavailable
			}
			return storageClient.AbortMultipartUpload(callContext, objectKey, uploadID)
		})
		cleanup := service.NewMultipartUploadSessionCandidateCleanup(aborter,
			func(callContext context.Context, objectKey, uploadID string) (*domain.MultipartUploadAbortObservation, error) {
				return storageClient.CheckMultipartUploadAfterAbort(callContext, objectKey, uploadID, verifySameInstance)
			})

		// 3. 调用既有单轮入口，保留边界、结果和继续位置。
		return service.NewMultipartUploadSessionCleanupRound(batch, cleanup).RunOnce(ctx, position, limits, gracePeriod)
	}, nil
}

func verifyMultipartUploadSessionCleanupInstance(
	ctx context.Context, endpoint string, verify storage.MultipartSessionLockVerifier,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if verify == nil {
		return "", nil
	}
	proofContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	instance, err := verify(proofContext, endpoint)
	if stopError := ctx.Err(); stopError != nil {
		return "", stopError
	}
	if err != nil || proofContext.Err() != nil {
		return "", domain.ErrUploadStorageUnavailable
	}
	return instance, nil
}
