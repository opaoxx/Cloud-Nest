package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// PurgeEntry 绑定永久清理幂等受理服务，路由须先挂载共享 Bearer 认证。
func PurgeEntry(beginPurge func(context.Context, domain.Actor, uint64, uint64, string, string) (json.RawMessage, error)) gin.HandlerFunc {
	// 1. 检查永久清理受理所需的服务依赖。
	if beginPurge == nil {
		panic("永久清理确认处理函数需要幂等受理服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 读取已认证身份，依次校验路径 ID 和幂等键。
		scope, valid := readEntryRevisionRequestScope(requestContext)
		if !valid {
			return
		}

		// 2. 解析预检版本和凭证请求头。
		input, fieldError := dto.ParsePurgeEntryHeaders(requestContext.Request.Header)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用幂等受理服务，并映射业务错误。
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		stableBody, err := beginPurge(ctx, scope.actor, scope.entryID, input.ExpectedRevision, input.PreviewDigest, scope.key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writePurgeEntryFailure(requestContext, err)
			return
		}

		// 4. 核对快照目标与请求路径一致，交付 HTTP 202。
		PurgeEntryAccepted(requestContext, scope.entryID, stableBody)
	}
}

func writePurgeEntryFailure(requestContext *gin.Context, err error) {
	// 1. 将预检凭证与范围问题映射为固定的公开错误。
	switch {
	case errors.Is(err, domain.ErrInvalidPurgePreviewDigest):
		InvalidArgument(requestContext, &dto.FieldError{
			Field: dto.PurgePreviewDigestHeader, Message: domain.ErrInvalidPurgePreviewDigest.Error(),
		})
	case errors.Is(err, domain.ErrPurgePreviewChanged):
		requestContext.AbortWithStatusJSON(http.StatusConflict, dto.ErrorResponse{
			Error:     dto.ErrorDetail{Code: "PURGE_PREVIEW_CHANGED", Message: domain.ErrPurgePreviewChanged.Error()},
			RequestID: requestcontext.RequestID(requestContext.Request.Context()),
		})
	default:
		// 2. 其余错误沿用目录项版本操作的公共映射。
		writeEntryRevisionFailure(requestContext, err, domain.ErrRootEntryPurge)
	}
}
