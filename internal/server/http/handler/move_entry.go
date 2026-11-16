package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// MoveEntry 绑定幂等移动服务，路由须先挂载认证中间件。
// move 绑定 IdempotentMoveEntryService.Move，身份来自认证上下文。
func MoveEntry(move func(context.Context, domain.Actor, uint64, uint64, uint64, string) (json.RawMessage, error)) gin.HandlerFunc {
	// 1. 检查服务依赖，构造每次请求的处理函数。
	if move == nil {
		panic("目录项移动处理函数需要幂等服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、请求取消和已认证身份。
		if requestContext.Writer.Written() {
			requestContext.Abort()
			return
		}
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		actor, exists := requestcontext.Actor(ctx)
		if !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 解析源路径及幂等请求头。
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 读取 JSON 正文并解析移动参数。
		var request dto.MoveEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseMoveEntryRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 4. 调用幂等服务，处理取消和领域错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := move(ctx, actor, path.EntryID, input.ExpectedRevision, input.TargetParentID, key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			response := dto.ErrorResponse{RequestID: requestcontext.RequestID(ctx)}
			status := http.StatusConflict
			switch {
			case errors.Is(err, domain.ErrFileEntryNotDirectory):
				InvalidArgument(requestContext, &dto.FieldError{Field: "target_parent_id", Message: "目标父目录须为文件夹"})
				return
			case errors.Is(err, domain.ErrEntryMoveIntoSubtree):
				InvalidArgument(requestContext, &dto.FieldError{Field: "target_parent_id", Message: domain.ErrEntryMoveIntoSubtree.Error()})
				return
			case errors.Is(err, domain.ErrFileEntryDepthExceeded):
				InvalidArgument(requestContext, &dto.FieldError{Field: "target_parent_id", Message: domain.ErrFileEntryDepthExceeded.Error()})
				return
			case errors.Is(err, domain.ErrFileEntrySubtreeTooLarge):
				InvalidArgument(requestContext, &dto.FieldError{Field: "id", Message: domain.ErrFileEntrySubtreeTooLarge.Error()})
				return
			case errors.Is(err, domain.ErrFileEntryNotFound):
				status = http.StatusNotFound
				response.Error = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error(), Details: nil}
			case errors.Is(err, domain.ErrRootEntryMove):
				status = http.StatusForbidden
				response.Error = dto.ErrorDetail{Code: "ROOT_OPERATION_FORBIDDEN", Message: domain.ErrRootEntryMove.Error(), Details: nil}
			case errors.Is(err, domain.ErrEntryRevisionConflict):
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict == nil || conflict.CurrentRevision == 0 {
					InternalError(requestContext, nil)
					return
				}
				response.Error = dto.ErrorDetail{Code: "REVISION_CONFLICT", Message: domain.ErrEntryRevisionConflict.Error(), Details: map[string]any{"current_revision": strconv.FormatUint(conflict.CurrentRevision, 10)}}
			case errors.Is(err, domain.ErrEntryNameConflict):
				response.Error = dto.ErrorDetail{Code: "NAME_CONFLICT", Message: domain.ErrEntryNameConflict.Error(), Details: nil}
			case errors.Is(err, domain.ErrIdempotencyRequestConflict):
				response.Error = dto.ErrorDetail{Code: "IDEMPOTENCY_CONFLICT", Message: domain.ErrIdempotencyRequestConflict.Error(), Details: nil}
			case errors.Is(err, domain.ErrIdempotencyRecordExpired):
				response.Error = dto.ErrorDetail{Code: "IDEMPOTENCY_EXPIRED", Message: domain.ErrIdempotencyRecordExpired.Error(), Details: nil}
			default:
				AuthorizationFailure(requestContext, err)
				return
			}
			requestContext.AbortWithStatusJSON(status, response)
			return
		}

		// 5. 核对返回快照与请求参数，交付成功响应。
		data, err := dto.RestoreMoveEntryData(result)
		nextRevision := input.ExpectedRevision
		if nextRevision < ^uint64(0) {
			nextRevision++
		}
		if err != nil || data.Entry.ID != strconv.FormatUint(path.EntryID, 10) || data.Entry.ParentID == nil || *data.Entry.ParentID != strconv.FormatUint(input.TargetParentID, 10) ||
			(data.Entry.Revision != strconv.FormatUint(input.ExpectedRevision, 10) && data.Entry.Revision != strconv.FormatUint(nextRevision, 10)) {
			InternalError(requestContext, nil)
			return
		}
		MoveEntrySuccess(requestContext, result)
	}
}
