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
	// 1. 向受信调用方核对当前 SDK 端点对应的实例。
	if ctx == nil || client == nil || client.sdkClient == nil {
		return false, domain.ErrUploadStorageUnavailable
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if verifyInstance == nil {
		return false, nil
	}
	checkContext, cancel := context.WithTimeout(ctx, 2*uploadPartsReadTimeout)
	defer cancel()
	endpoint := client.sdkClient.EndpointURL().String()
	before, err := verifyInstance(checkContext, endpoint)
	if contextError := checkContext.Err(); contextError != nil {
		return false, contextError
	}
	if err != nil {
		return false, domain.ErrUploadStorageUnavailable
	}
	if before == "" {
		return false, nil
	}

	// 2. 对原会话执行 ListParts，再核对原对象的 HEAD。
	_, err = client.listUploadPartsPage(checkContext, objectKey, uploadID, 0, 1, true)
	if !errors.Is(err, domain.ErrUploadPartSessionMissing) {
		return false, err
	}
	_, err = client.StatMultipartUploadObject(checkContext, objectKey)
	if !errors.Is(err, domain.ErrUploadObjectMissing) {
		return false, err
	}

	// 3. 再次核对实例身份，前后相符才确认缺失。
	after, err := verifyInstance(checkContext, endpoint)
	if contextError := checkContext.Err(); contextError != nil {
		return false, contextError
	}
	if err != nil {
		return false, domain.ErrUploadStorageUnavailable
	}
	return after == before, nil
}
