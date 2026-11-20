package handler

import (
	"context"
	"encoding/json"
	"math"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// TrashEntry 绑定幂等回收服务，路由须先挂载认证中间件。
func TrashEntry(trash func(context.Context, domain.Actor, uint64, uint64, string) (json.RawMessage, error)) gin.HandlerFunc {
	// 1. 检查回收服务依赖。
	if trash == nil {
		panic("目录项回收处理函数需要幂等服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态并读取已认证身份。
		scope, valid := readEntryRevisionRequestScope(requestContext)
		if !valid {
			return
		}

		// 2. 路径与幂等键通过检查后，解析受限 JSON 正文。
		var request dto.TrashEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseTrashEntryRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用幂等回收服务，映射错误或交付成功快照。
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := trash(ctx, scope.actor, scope.entryID, input.ExpectedRevision, scope.key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeEntryRevisionFailure(requestContext, err, domain.ErrRootEntryTrash)
			return
		}
		data, err := dto.RestoreTrashEntryData(result)
		if err != nil || input.ExpectedRevision == math.MaxUint64 ||
			data.Entry.ID != strconv.FormatUint(scope.entryID, 10) ||
			data.Entry.Revision != strconv.FormatUint(input.ExpectedRevision+1, 10) {
			InternalError(requestContext, nil)
			return
		}
		TrashEntrySuccess(requestContext, result)
	}
}
