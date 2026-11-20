package handler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RestoreEntry 绑定幂等恢复服务，路由须先挂载认证中间件。
func RestoreEntry(restore func(context.Context, domain.Actor, uint64, uint64, string) (json.RawMessage, error)) gin.HandlerFunc {
	// 1. 检查恢复服务依赖。
	if restore == nil {
		panic("目录项恢复处理函数需要幂等服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，读取已认证用户。
		scope, valid := readEntryRevisionRequestScope(requestContext)
		if !valid {
			return
		}

		// 2. 目标 ID 和幂等键通过检查后，解析受限 JSON 和预期版本。
		var request dto.RestoreEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseRestoreEntryRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用幂等恢复服务，将业务错误映射为 HTTP 响应。
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := restore(ctx, scope.actor, scope.entryID, input.ExpectedRevision, scope.key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeRestoreEntryFailure(requestContext, err)
			return
		}

		// 4. 核对成功快照的目标 ID 和新版本，再交付 HTTP 200。
		data, err := dto.DecodeRestoreEntryData(result)
		if err != nil || input.ExpectedRevision == math.MaxUint64 ||
			data.Entry.ID != strconv.FormatUint(scope.entryID, 10) ||
			data.Entry.Revision != strconv.FormatUint(input.ExpectedRevision+1, 10) {
			InternalError(requestContext, nil)
			return
		}
		RestoreEntrySuccess(requestContext, result)
	}
}

func writeRestoreEntryFailure(requestContext *gin.Context, err error) {
	// 1. 映射恢复特有的冲突，其余业务错误沿用共用规则。
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrEntryNameConflict):
		detail = dto.ErrorDetail{Code: "NAME_CONFLICT", Message: domain.ErrEntryNameConflict.Error()}
	case errors.Is(err, domain.ErrRestoreTargetUnavailable):
		detail = dto.ErrorDetail{Code: "TARGET_UNAVAILABLE", Message: domain.ErrRestoreTargetUnavailable.Error()}
	default:
		writeEntryRevisionFailure(requestContext, err, domain.ErrRootEntryRestore)
		return
	}

	// 2. 返回固定的冲突响应，附加当次请求 ID。
	requestContext.AbortWithStatusJSON(http.StatusConflict, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
