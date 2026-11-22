package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// readReuseFileRequest 读取秒传的幂等键和正文，失败时写出错误并停止处理。
func readReuseFileRequest(requestContext *gin.Context) (*dto.ReuseFileInput, string) {
	// 1. 使用现有规则读取单个幂等键。
	key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return nil, ""
	}

	// 2. 沿用共享 JSON 绑定，再按字段校验秒传声明。
	var request dto.ReuseFileRequest
	if !binding.ReadJSON(requestContext, &request) {
		return nil, ""
	}
	input, fieldError := dto.ParseReuseFileRequest(request)
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return nil, ""
	}

	// 3. 只有请求头和正文都合法时才交付调用参数。
	return input, key
}
