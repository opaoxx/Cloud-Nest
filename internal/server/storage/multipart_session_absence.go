package storage

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartSessionLockVerifier 由受信的部署侧确认端点直连具备同会话锁语义的存储实例。
// 返回一次运行实例的稳定标识，重启或替换后标识必须变化。空标识表示未确认。
type MultipartSessionLockVerifier func(ctx context.Context, endpoint string) (instanceID string, err error)

// ConfirmMultipartSessionAbsence 在同一受信实例上重新核对原会话及对象均缺失。
// 调用方仍须复查任务租约及存储绑定。此方法仅访问存储，不更新任务或配额。
func (client *Client) ConfirmMultipartSessionAbsence(
	ctx context.Context, objectKey, uploadID string, verifyInstance MultipartSessionLockVerifier,
) (bool, error) {
	observation, err := client.checkClosedMultipartSession(ctx, objectKey, uploadID, verifyInstance, false)
	return observation != nil, err
}

// MultipartUploadAbortObservation 保存已关闭原会话后的对象观察。
// Object 为 nil 表示会话和对象都明确缺失。非空时保留 HEAD 返回的原对象大小、ETag 和存储版本。
type MultipartUploadAbortObservation struct {
	Object *domain.UploadObjectResult
}

// CheckMultipartUploadAfterAbort 只读复查中止后的原会话及原对象。
// 受信实例须保证迟到的分片写回与合并无法重建已关闭的原会话。
// nil, nil 表示会话仍存在或当前实例无法确认。调用方后续仍须复核任务租约和存储绑定。
func (client *Client) CheckMultipartUploadAfterAbort(
	ctx context.Context, objectKey, uploadID string, verifyInstance MultipartSessionLockVerifier,
) (*MultipartUploadAbortObservation, error) {
	return client.checkClosedMultipartSession(ctx, objectKey, uploadID, verifyInstance, true)
}

func (client *Client) checkClosedMultipartSession(
	ctx context.Context, objectKey, uploadID string, verifyInstance MultipartSessionLockVerifier, includeObject bool,
) (*MultipartUploadAbortObservation, error) {
	// 1. 向受信调用方核对内部端点对应的实例。
	if ctx == nil || client == nil || client.sdkClient == nil {
		return nil, domain.ErrUploadStorageUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifyInstance == nil {
		return nil, nil
	}
	checkContext, cancel := context.WithTimeout(ctx, 2*uploadPartsReadTimeout)
	defer cancel()
	endpoint := client.sdkClient.EndpointURL().String()
	before, err := verifyInstance(checkContext, endpoint)
	if contextError := checkContext.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, domain.ErrUploadStorageUnavailable
	}
	if before == "" {
		return nil, nil
	}

	// 2. 先查询原会话。只有存储明确报告缺失后，才查询原对象。
	_, err = client.listUploadPartsPage(checkContext, objectKey, uploadID, 0, 1, true)
	if !errors.Is(err, domain.ErrUploadPartSessionMissing) {
		return nil, err
	}
	object, err := client.StatMultipartUploadObject(checkContext, objectKey)
	if err != nil && !errors.Is(err, domain.ErrUploadObjectMissing) {
		return nil, err
	}
	if object != nil && !includeObject {
		return nil, nil
	}

	// 3. 再核对实例身份，前后相符才交付会话缺失及原对象的观察。
	after, err := verifyInstance(checkContext, endpoint)
	if contextError := checkContext.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, domain.ErrUploadStorageUnavailable
	}
	if after != before {
		return nil, nil
	}
	return &MultipartUploadAbortObservation{Object: object}, nil
}
